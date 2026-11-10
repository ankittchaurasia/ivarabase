package core

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- cron

// cronSpec is a standard 5-field cron expression (UTC):
// minute hour day-of-month month day-of-week, with *, lists, ranges and steps.
type cronSpec struct {
	min, hour, dom, mon, dow uint64 // bitsets
	domStar, dowStar         bool
}

func parseCronField(f string, lo, hi int) (uint64, bool, error) {
	var bits uint64
	star := f == "*"
	for _, part := range strings.Split(f, ",") {
		step := 1
		if i := strings.Index(part, "/"); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return 0, false, fmt.Errorf("bad step in %q", f)
			}
			step, part = n, part[:i]
		}
		a, b := lo, hi
		switch {
		case part == "*":
		case strings.Contains(part, "-"):
			x, y, _ := strings.Cut(part, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(x)
			b, err2 = strconv.Atoi(y)
			if err1 != nil || err2 != nil {
				return 0, false, fmt.Errorf("bad range in %q", f)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return 0, false, fmt.Errorf("bad value %q", part)
			}
			a, b = n, n
			if step > 1 {
				b = hi
			}
		}
		if hi == 7 { // day of week: 7 is Sunday too
			if a == 7 {
				a = 0
			}
			if b == 7 {
				bits |= 1
				b = 6
			}
		}
		if a < lo || b > hi || a > b {
			return 0, false, fmt.Errorf("%q is out of range %d-%d", f, lo, hi)
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, star, nil
}

func parseCron(expr string) (*cronSpec, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron needs 5 fields (minute hour day month weekday), got %d", len(f))
	}
	c := &cronSpec{}
	var err error
	if c.min, _, err = parseCronField(f[0], 0, 59); err != nil {
		return nil, err
	}
	if c.hour, _, err = parseCronField(f[1], 0, 23); err != nil {
		return nil, err
	}
	if c.dom, c.domStar, err = parseCronField(f[2], 1, 31); err != nil {
		return nil, err
	}
	if c.mon, _, err = parseCronField(f[3], 1, 12); err != nil {
		return nil, err
	}
	if c.dow, c.dowStar, err = parseCronField(f[4], 0, 7); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cronSpec) matches(t time.Time) bool {
	t = t.UTC()
	if c.min&(1<<uint(t.Minute())) == 0 || c.hour&(1<<uint(t.Hour())) == 0 || c.mon&(1<<uint(t.Month())) == 0 {
		return false
	}
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default: // classic cron: either restricted field may match
		return domOK || dowOK
	}
}

// next returns the next minute after t that matches (searches up to ~1 year).
func (c *cronSpec) next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if c.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// cronLoop runs scheduled functions once per matching minute.
func (a *App) cronLoop(ctx context.Context) {
	for {
		now := time.Now()
		wait := now.Truncate(time.Minute).Add(time.Minute).Sub(now) + 200*time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		minute := time.Now().UTC().Truncate(time.Minute)
		rows, err := a.Meta.Query(`SELECT project, name, schedule FROM functions WHERE schedule <> ''`)
		if err != nil {
			continue
		}
		type job struct{ project, name string }
		var due []job
		for rows.Next() {
			var p, n, s string
			rows.Scan(&p, &n, &s)
			if cs, err := parseCron(s); err == nil && cs.matches(minute) {
				due = append(due, job{p, n})
			}
		}
		rows.Close()
		for _, j := range due {
			go a.runScheduled(ctx, j.project, j.name)
		}
	}
}

func (a *App) runScheduled(ctx context.Context, project, name string) {
	fn, err := a.function(project, name)
	if err != nil {
		return
	}
	st, err := a.projectAPIState(project)
	if err != nil {
		return
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://function/", strings.NewReader(`{"scheduled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey(st.Secret, project, roleService))
	req.Header.Set("X-Ivara-Cron", fn.Schedule)
	rec := newMemResponse()
	a.serveFunction(rec, req, fn, "/", true)
	a.Meta.Exec(`UPDATE functions SET last_run_at=now() WHERE project=$1 AND name=$2`, project, name)
	if rec.status >= 400 {
		log.Printf("scheduled function %s/%s returned %d", project, name, rec.status)
	}
}

// ---------------------------------------------------------------- console capture

type consoleLine struct {
	project, name, stream, line string
	at                          time.Time
}

type consoleSink struct {
	once sync.Once
	ch   chan consoleLine
	mu   sync.Mutex
	buf  []consoleLine
}

var sink = &consoleSink{}

// console queues one line of a function's output; a background writer
// stores them in batches so a chatty function never blocks on the database.
func (f *FnRuntime) console(project, name, stream, line string) {
	if len(line) > 4000 {
		line = line[:4000] + "…"
	}
	sink.once.Do(func() {
		sink.ch = make(chan consoleLine, 4096)
		go func() {
			t := time.NewTicker(time.Second)
			for {
				select {
				case l := <-sink.ch:
					sink.mu.Lock()
					sink.buf = append(sink.buf, l)
					sink.mu.Unlock()
				case <-t.C:
					f.flushConsole()
				}
			}
		}()
	})
	select {
	case sink.ch <- consoleLine{project, name, stream, line, time.Now()}:
	default: // drop when flooded
	}
}

func (f *FnRuntime) flushConsole() {
	// drain anything still queued in the channel first
	if sink.ch != nil {
		for {
			select {
			case l := <-sink.ch:
				sink.mu.Lock()
				sink.buf = append(sink.buf, l)
				sink.mu.Unlock()
				continue
			default:
			}
			break
		}
	}
	sink.mu.Lock()
	batch := sink.buf
	sink.buf = nil
	sink.mu.Unlock()
	for _, l := range batch {
		f.app.Meta.Exec(`INSERT INTO function_console(project,name,at,stream,line) VALUES ($1,$2,$3,$4,$5)`, l.project, l.name, l.at, l.stream, l.line)
	}
}

// lineWriter turns a byte stream into lines.
type lineWriter struct {
	fn  func(string)
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimRight(string(w.buf[:i]), "\r"); line != "" {
			w.fn(line)
		}
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 8192 {
		w.fn(string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}

// ---------------------------------------------------------------- /functions/v1 route

// serveProjectFunction handles <project url>/functions/v1/<name>/... the way
// Supabase does: with verify_jwt the caller must present a JWT signed with the
// project secret (anon key, user session or service key); the Authorization
// header is passed to the function so it can act as that user.
func (s *Server) serveProjectFunction(w http.ResponseWriter, r *http.Request, slug string, secret []byte, rest string) {
	name, sub, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	fn, err := s.app.function(slug, name)
	if err != nil {
		supaError(w, 404, "Function not found", "deploy it first")
		return
	}
	if fn.VerifyJWT {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" {
			tok = r.Header.Get("apikey")
		}
		if _, err := verifyJWT(secret, tok); err != nil {
			supaError(w, 401, "Invalid JWT", "send Authorization: Bearer <anon key or user access token>, or deploy with verify_jwt=false")
			return
		}
	}
	s.app.metrics.count(slug, "functions")
	s.app.serveFunction(w, r, fn, "/"+sub, true)
}
