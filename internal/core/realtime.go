package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Realtime: Supabase's Phoenix-channel protocol (vsn 1.0.0, JSON messages
// {topic, event, payload, ref}) at /realtime/v1/websocket under the project
// URL, so supabase-js `supabase.channel(...)` works unchanged. Broadcast and
// presence live in memory here; postgres_changes are in realtime_pg.go.
//
// Isolation: every connection belongs to exactly one project (the one whose
// key it presented); topics, presence and database changes are kept per
// project, so a channel name can never reach another project's clients.

const (
	rtMaxConnsPerProject = 500
	rtMaxConnsTotal      = 10000
	rtMaxChannelsPerConn = 100
	rtMaxPGSubsPerChan   = 100
	rtConnRate           = 100 // client messages per second per connection
	rtConnBurst          = 200
	rtProjectRate        = 2000 // client + REST messages per second per project
	rtProjectBurst       = 4000
	rtOutQueue           = 1024             // queued outgoing messages before a slow client is dropped
	rtIdleTimeout        = 75 * time.Second // supabase-js sends a heartbeat every 30s
	rtMaxRESTMessages    = 100
)

type rtHub struct {
	mu       sync.Mutex
	projects map[string]*rtProject
	total    int
}

var rtHubs = &rtHub{projects: map[string]*rtProject{}}

type rtProject struct {
	slug string
	app  *App

	mu     sync.Mutex
	conns  map[*rtConn]struct{}
	topics map[string]map[*rtChan]struct{}
	rate   rtBucket

	pipeMu sync.Mutex
	pipe   *rtPipeline

	secretMu sync.Mutex
	secret   []byte

	msgsIn, msgsOut, dropped atomic.Int64
}

type rtConn struct {
	p       *rtProject
	ws      *wsConn
	id      string
	ip      string
	ua      string
	apikey  string
	started time.Time
	out     chan []byte
	done    chan struct{}
	once    sync.Once
	rate    rtBucket // guarded by p.mu
	chans   map[string]*rtChan
}

type rtChan struct {
	c       *rtConn
	topic   string
	joinRef string
	self    bool
	ack     bool
	private bool
	presKey string
	pres    map[string]any // tracked presence meta, nil when not tracking
	presRef string
	claims  map[string]any
	role    string
	timer   *time.Timer
	pg      []*rtPGSub
	joined  time.Time
}

// project returns (creating) the hub entry for slug.
func (h *rtHub) project(a *App, slug string) *rtProject {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.projects[slug]
	if p == nil {
		p = &rtProject{slug: slug, app: a, conns: map[*rtConn]struct{}{}, topics: map[string]map[*rtChan]struct{}{},
			rate: rtBucket{rate: rtProjectRate, burst: rtProjectBurst}}
		h.projects[slug] = p
	}
	return p
}

func (h *rtHub) get(slug string) *rtProject {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.projects[slug]
}

// realtimeDropProject disconnects every client of a project (deleted project,
// rotated JWT secret) and stops its change feed.
func realtimeDropProject(slug string) {
	realtimeReady.Delete(slug)
	rtHubs.mu.Lock()
	p := rtHubs.projects[slug]
	delete(rtHubs.projects, slug)
	rtHubs.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	conns := make([]*rtConn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		c.kill(wsCloseGoingAway, "project credentials changed")
	}
	p.stopPipeline()
	p.app.closeProjectDB(slug) // a drain racing the delete may have reopened it
}

// rtBucket is a token bucket; callers hold the owning lock.
type rtBucket struct {
	tokens, rate, burst float64
	last                time.Time
}

func (b *rtBucket) allow(now time.Time) bool {
	if b.last.IsZero() {
		b.tokens = b.burst
	} else {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ---------------------------------------------------------------- HTTP entry

// serveRealtime handles /proj/<slug>/realtime/v1/<sub>; the gateway has
// already checked that apikey is a valid key for this project.
func (a *App) serveRealtime(w http.ResponseWriter, r *http.Request, slug, sub, apikey string) {
	switch strings.TrimRight(sub, "/") {
	case "/websocket":
		a.realtimeSocket(w, r, slug, apikey)
	case "/api/broadcast":
		if r.Method != http.MethodPost {
			supaError(w, 405, "use POST", "")
			return
		}
		a.realtimeBroadcastREST(w, r, slug, apikey)
	case "", "/api/health", "/health":
		writeJSON(w, 200, map[string]any{"name": "realtime", "version": "ivarabase-" + Version})
	default:
		supaError(w, 404, "Unknown realtime endpoint "+sub, "connect supabase-js to the project URL; realtime lives at /realtime/v1/websocket")
	}
}

func (a *App) realtimeSocket(w http.ResponseWriter, r *http.Request, slug, apikey string) {
	if v := r.URL.Query().Get("vsn"); v != "" && v != "1.0.0" {
		supaError(w, 400, "unsupported realtime protocol version "+v, "this server speaks vsn 1.0.0 (JSON)")
		return
	}
	p := rtHubs.project(a, slug)
	rtHubs.mu.Lock()
	full := rtHubs.total >= rtMaxConnsTotal
	rtHubs.mu.Unlock()
	p.mu.Lock()
	if !full && len(p.conns) >= rtMaxConnsPerProject {
		full = true
	}
	p.mu.Unlock()
	if full {
		supaError(w, 429, "Too many realtime connections", fmt.Sprintf("a project may hold %d concurrent realtime connections", rtMaxConnsPerProject))
		return
	}
	ws, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	c := &rtConn{p: p, ws: ws, id: randHex(8), ip: a.clientIP(r), ua: r.UserAgent(), apikey: apikey, started: time.Now(),
		out: make(chan []byte, rtOutQueue), done: make(chan struct{}), chans: map[string]*rtChan{},
		rate: rtBucket{rate: rtConnRate, burst: rtConnBurst}}
	rtHubs.mu.Lock()
	rtHubs.total++
	rtHubs.mu.Unlock()
	p.mu.Lock()
	p.conns[c] = struct{}{}
	p.mu.Unlock()
	go c.writer()
	c.reader()
}

func (c *rtConn) writer() {
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			if err := c.ws.WriteText(b); err != nil {
				c.kill(wsCloseGoingAway, "")
				return
			}
		}
	}
}

// send queues a message without blocking; a client that can't keep up is dropped.
func (c *rtConn) send(b []byte) {
	select {
	case <-c.done:
	case c.out <- b:
		c.p.msgsOut.Add(1)
	default:
		c.p.dropped.Add(1)
		go c.kill(wsClosePolicy, "client too slow")
	}
}

func (c *rtConn) kill(code int, reason string) {
	c.once.Do(func() {
		close(c.done)
		c.ws.Close(code, reason)
	})
}

func (c *rtConn) reader() {
	defer func() {
		c.kill(wsCloseNormal, "")
		c.cleanup()
	}()
	for {
		c.ws.SetReadDeadline(time.Now().Add(rtIdleTimeout))
		op, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if op != wsOpText {
			continue
		}
		var m rtIn
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		c.p.msgsIn.Add(1)
		now := time.Now()
		c.p.mu.Lock()
		ok := c.rate.allow(now) && c.p.rate.allow(now)
		c.p.mu.Unlock()
		if !ok {
			c.p.dropped.Add(1)
			if m.Ref != nil {
				c.reply(m.Topic, *m.Ref, "error", map[string]any{"reason": "rate limit exceeded: too many messages"})
			}
			continue
		}
		c.handle(&m)
	}
}

func (c *rtConn) cleanup() {
	p := c.p
	p.mu.Lock()
	for _, ch := range c.chans {
		p.removeChanLocked(ch)
	}
	_, was := p.conns[c]
	delete(p.conns, c)
	p.mu.Unlock()
	if was {
		rtHubs.mu.Lock()
		rtHubs.total--
		rtHubs.mu.Unlock()
	}
}

// ---------------------------------------------------------------- protocol

type rtIn struct {
	Topic   string          `json:"topic"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
	Ref     *string         `json:"ref"`
}

type rtOut struct {
	Topic   string `json:"topic"`
	Event   string `json:"event"`
	Payload any    `json:"payload"`
	Ref     any    `json:"ref"`
}

func rtEncode(topic, event string, payload any, ref any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rtOut{Topic: topic, Event: event, Payload: payload, Ref: ref}); err != nil {
		b, _ := json.Marshal(rtOut{Topic: topic, Event: event, Payload: map[string]any{}, Ref: ref})
		return b
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func refOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (c *rtConn) reply(topic, ref, status string, response any) {
	if response == nil {
		response = map[string]any{}
	}
	c.send(rtEncode(topic, "phx_reply", map[string]any{"status": status, "response": response}, refOrNil(ref)))
}

// system sends a realtime "system" message for a channel.
func (ch *rtChan) system(extension, status, message string) {
	ch.c.send(rtEncode(ch.topic, "system", map[string]any{
		"channel": strings.TrimPrefix(ch.topic, "realtime:"), "extension": extension, "status": status, "message": message,
	}, nil))
}

func (c *rtConn) handle(m *rtIn) {
	ref := ""
	if m.Ref != nil {
		ref = *m.Ref
	}
	if m.Topic == "phoenix" && m.Event == "heartbeat" {
		c.reply("phoenix", ref, "ok", nil)
		return
	}
	switch m.Event {
	case "phx_join":
		c.join(m.Topic, ref, m.Payload)
		return
	}
	p := c.p
	p.mu.Lock()
	ch := c.chans[m.Topic]
	p.mu.Unlock()
	if ch == nil {
		if m.Event != "phx_leave" && m.Event != "access_token" {
			c.reply(m.Topic, ref, "error", map[string]any{"reason": "unmatched topic"})
		} else if m.Event == "phx_leave" {
			c.reply(m.Topic, ref, "ok", nil)
		}
		return
	}
	switch m.Event {
	case "phx_leave":
		p.mu.Lock()
		p.removeChanLocked(ch)
		p.mu.Unlock()
		c.reply(m.Topic, ref, "ok", nil)
		c.send(rtEncode(m.Topic, "phx_close", map[string]any{}, refOrNil(ch.joinRef)))
	case "access_token":
		var body struct {
			AccessToken *string `json:"access_token"`
		}
		json.Unmarshal(m.Payload, &body)
		if body.AccessToken == nil || *body.AccessToken == "" {
			return
		}
		c.refreshToken(ch, *body.AccessToken)
	case "broadcast":
		p.broadcast(m.Topic, m.Payload, ch)
		if ch.ack {
			c.reply(m.Topic, ref, "ok", nil)
		}
	case "presence":
		var body struct {
			Event   string         `json:"event"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(m.Payload, &body); err != nil {
			c.reply(m.Topic, ref, "error", map[string]any{"reason": "invalid presence payload"})
			return
		}
		switch body.Event {
		case "track":
			if body.Payload == nil {
				body.Payload = map[string]any{}
			}
			p.track(ch, body.Payload)
		case "untrack":
			p.untrack(ch)
		default:
			c.reply(m.Topic, ref, "error", map[string]any{"reason": "unknown presence event " + body.Event})
			return
		}
		c.reply(m.Topic, ref, "ok", nil)
	default:
		c.reply(m.Topic, ref, "error", map[string]any{"reason": "unknown event " + m.Event})
	}
}

// claimsFor verifies a token for this project. User access tokens from
// /auth/v1 carry no "ref" claim; anon/service keys must name this project.
func (p *rtProject) claimsFor(token string) (map[string]any, string, error) {
	secret, err := p.jwtSecret()
	if err != nil {
		return nil, "", err
	}
	claims, err := verifyJWT(secret, token)
	if err != nil {
		if strings.Contains(err.Error(), "expired") {
			return nil, "", fmt.Errorf("Token has expired")
		}
		return nil, "", fmt.Errorf("Invalid JWT: unable to verify the token signature")
	}
	if ref, ok := claims["ref"].(string); ok && ref != p.slug {
		return nil, "", fmt.Errorf("Invalid JWT: token is for another project")
	}
	role, _ := claims["role"].(string)
	switch role {
	case roleAnon, roleAuthed, roleService:
	default:
		return nil, "", fmt.Errorf("Invalid JWT: unsupported role %q", role)
	}
	return claims, role, nil
}

// jwtSecret is loaded once per hub entry; rotating the secret drops the
// entry (realtimeDropProject), so a stale secret is never used.
func (p *rtProject) jwtSecret() ([]byte, error) {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.secret == nil {
		st, err := p.app.projectAPIState(p.slug)
		if err != nil {
			return nil, err
		}
		p.secret = st.Secret
	}
	return p.secret, nil
}

type rtJoinPayload struct {
	Config struct {
		Broadcast struct {
			Self bool `json:"self"`
			Ack  bool `json:"ack"`
		} `json:"broadcast"`
		Presence struct {
			Key string `json:"key"`
		} `json:"presence"`
		PostgresChanges []map[string]any `json:"postgres_changes"`
		Private         bool             `json:"private"`
	} `json:"config"`
	AccessToken string `json:"access_token"`
}

func (c *rtConn) join(topic, ref string, raw json.RawMessage) {
	p := c.p
	var jp rtJoinPayload
	if len(raw) > 0 && json.Unmarshal(raw, &jp) != nil {
		c.reply(topic, ref, "error", map[string]any{"reason": "invalid join payload"})
		return
	}
	if topic == "" || len(topic) > 512 {
		c.reply(topic, ref, "error", map[string]any{"reason": "invalid topic"})
		return
	}
	tok := jp.AccessToken
	if tok == "" {
		tok = c.apikey
	}
	claims, role, err := p.claimsFor(tok)
	if err != nil {
		c.reply(topic, ref, "error", map[string]any{"reason": err.Error()})
		return
	}
	if jp.Config.Private && role != roleService {
		c.reply(topic, ref, "error", map[string]any{"reason": "private channels are not supported on this server yet; use a public channel or the service_role key"})
		return
	}
	subs, filters, err := parsePGSubs(jp.Config.PostgresChanges)
	if err != nil {
		c.reply(topic, ref, "error", map[string]any{"reason": err.Error()})
		return
	}
	ch := &rtChan{c: c, topic: topic, joinRef: ref, self: jp.Config.Broadcast.Self, ack: jp.Config.Broadcast.Ack,
		private: jp.Config.Private, presKey: jp.Config.Presence.Key, claims: claims, role: role, pg: subs, joined: time.Now()}
	if ch.presKey == "" {
		ch.presKey = randUUID()
	}
	for _, s := range subs {
		s.ch = ch
	}
	// Start the change feed before answering, so a change made right after
	// SUBSCRIBED is not missed.
	var pgErr error
	if len(subs) > 0 {
		pgErr = p.ensurePipeline()
	}

	p.mu.Lock()
	if old := c.chans[topic]; old != nil {
		p.removeChanLocked(old) // a rejoin replaces the previous membership
	}
	if len(c.chans) >= rtMaxChannelsPerConn {
		p.mu.Unlock()
		c.reply(topic, ref, "error", map[string]any{"reason": fmt.Sprintf("too many channels on one connection (max %d)", rtMaxChannelsPerConn)})
		return
	}
	c.chans[topic] = ch
	if p.topics[topic] == nil {
		p.topics[topic] = map[*rtChan]struct{}{}
	}
	p.topics[topic][ch] = struct{}{}
	ch.armExpiry()
	c.reply(topic, ref, "ok", map[string]any{"postgres_changes": filters})
	c.send(rtEncode(topic, "presence_state", p.presenceStateLocked(topic), nil))
	p.mu.Unlock()

	if len(subs) > 0 {
		if pgErr != nil {
			ch.system("postgres_changes", "error", "Unable to subscribe to changes: "+pgErr.Error())
		} else {
			ch.system("postgres_changes", "ok", "Subscribed to PostgreSQL")
		}
	}
}

// armExpiry closes the channel when its token expires, like Supabase. p.mu held.
func (ch *rtChan) armExpiry() {
	if ch.timer != nil {
		ch.timer.Stop()
		ch.timer = nil
	}
	exp, ok := ch.claims["exp"].(float64)
	if !ok {
		return
	}
	d := time.Until(time.Unix(int64(exp), 0))
	ch.timer = time.AfterFunc(d, func() { ch.c.p.expire(ch, "Token has expired") })
}

func (p *rtProject) expire(ch *rtChan, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch.c.chans[ch.topic] != ch {
		return
	}
	ch.system("system", "error", msg)
	ch.c.send(rtEncode(ch.topic, "phx_close", map[string]any{}, refOrNil(ch.joinRef)))
	p.removeChanLocked(ch)
}

func (c *rtConn) refreshToken(ch *rtChan, tok string) {
	p := c.p
	claims, role, err := p.claimsFor(tok)
	if err != nil {
		p.expire(ch, err.Error())
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.chans[ch.topic] != ch {
		return
	}
	if ch.private && role != roleService {
		ch.system("system", "error", "private channels need the service_role key")
		ch.c.send(rtEncode(ch.topic, "phx_close", map[string]any{}, refOrNil(ch.joinRef)))
		p.removeChanLocked(ch)
		return
	}
	ch.claims, ch.role = claims, role
	ch.armExpiry()
}

// removeChanLocked drops a channel membership and announces its presence leave. p.mu held.
func (p *rtProject) removeChanLocked(ch *rtChan) {
	if ch.timer != nil {
		ch.timer.Stop()
	}
	if ch.c.chans[ch.topic] == ch {
		delete(ch.c.chans, ch.topic)
	}
	if set := p.topics[ch.topic]; set != nil {
		delete(set, ch)
		if len(set) == 0 {
			delete(p.topics, ch.topic)
		}
	}
	if ch.pres != nil {
		leaves := map[string]any{ch.presKey: map[string]any{"metas": []any{ch.presMetaLocked()}}}
		ch.pres = nil
		p.presenceDiffLocked(ch.topic, map[string]any{}, leaves)
	}
}

// ---------------------------------------------------------------- broadcast

// broadcast fans a client broadcast out to the topic; payload is the
// original {type, event, payload} object.
func (p *rtProject) broadcast(topic string, payload json.RawMessage, from *rtChan) {
	msg := rtEncode(topic, "broadcast", payload, nil)
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.topics[topic] {
		if ch == from && !ch.self {
			continue
		}
		ch.c.send(msg)
	}
}

// realtimeBroadcastREST is POST /realtime/v1/api/broadcast, which supabase-js
// uses for channel.send() when the channel is not subscribed.
func (a *App) realtimeBroadcastREST(w http.ResponseWriter, r *http.Request, slug, apikey string) {
	p := rtHubs.project(a, slug)
	tok := bearer(r)
	if tok == "" {
		tok = apikey
	}
	_, role, err := p.claimsFor(tok)
	if err != nil {
		supaError(w, 401, err.Error(), "")
		return
	}
	var body struct {
		Messages []struct {
			Topic   string          `json:"topic"`
			Event   string          `json:"event"`
			Payload json.RawMessage `json:"payload"`
			Private bool            `json:"private"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, wsMaxMessage)).Decode(&body); err != nil {
		supaError(w, 400, "invalid JSON body: send {\"messages\":[{\"topic\",\"event\",\"payload\"}]}", "")
		return
	}
	if len(body.Messages) == 0 || len(body.Messages) > rtMaxRESTMessages {
		supaError(w, 400, fmt.Sprintf("send between 1 and %d messages", rtMaxRESTMessages), "")
		return
	}
	for _, m := range body.Messages {
		if m.Topic == "" || m.Event == "" {
			supaError(w, 400, "every message needs a topic and an event", "")
			return
		}
		if m.Private && role != roleService {
			supaError(w, 403, "private channels are not supported on this server yet", "use a public channel or the service_role key")
			return
		}
	}
	now := time.Now()
	p.mu.Lock()
	ok := true
	for range body.Messages {
		if !p.rate.allow(now) {
			ok = false
			break
		}
	}
	p.mu.Unlock()
	if !ok {
		supaError(w, 429, "Too many realtime messages", "this project is over its realtime message rate")
		return
	}
	for _, m := range body.Messages {
		pl := m.Payload
		if len(pl) == 0 {
			pl = json.RawMessage("{}")
		}
		inner, _ := json.Marshal(map[string]any{"type": "broadcast", "event": m.Event, "payload": pl})
		p.msgsIn.Add(1)
		p.broadcast("realtime:"+m.Topic, inner, nil)
	}
	writeJSON(w, 202, map[string]any{"message": "Broadcast sent"})
}

// ---------------------------------------------------------------- presence

func (ch *rtChan) presMetaLocked() map[string]any {
	meta := map[string]any{}
	for k, v := range ch.pres {
		meta[k] = v
	}
	meta["phx_ref"] = ch.presRef
	return meta
}

func (p *rtProject) track(ch *rtChan, payload map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch.c.chans[ch.topic] != ch {
		return
	}
	leaves := map[string]any{}
	prev := ""
	if ch.pres != nil {
		leaves[ch.presKey] = map[string]any{"metas": []any{ch.presMetaLocked()}}
		prev = ch.presRef
	}
	ch.pres = payload
	ch.presRef = randHex(8)
	meta := ch.presMetaLocked()
	if prev != "" {
		meta["phx_ref_prev"] = prev
	}
	p.presenceDiffLocked(ch.topic, map[string]any{ch.presKey: map[string]any{"metas": []any{meta}}}, leaves)
}

func (p *rtProject) untrack(ch *rtChan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch.pres == nil || ch.c.chans[ch.topic] != ch {
		return
	}
	leaves := map[string]any{ch.presKey: map[string]any{"metas": []any{ch.presMetaLocked()}}}
	ch.pres = nil
	p.presenceDiffLocked(ch.topic, map[string]any{}, leaves)
}

func (p *rtProject) presenceDiffLocked(topic string, joins, leaves map[string]any) {
	msg := rtEncode(topic, "presence_diff", map[string]any{"joins": joins, "leaves": leaves}, nil)
	for ch := range p.topics[topic] {
		ch.c.send(msg)
	}
}

func (p *rtProject) presenceStateLocked(topic string) map[string]any {
	state := map[string][]any{}
	for ch := range p.topics[topic] {
		if ch.pres != nil {
			state[ch.presKey] = append(state[ch.presKey], ch.presMetaLocked())
		}
	}
	out := map[string]any{}
	for k, metas := range state {
		out[k] = map[string]any{"metas": metas}
	}
	return out
}

// ---------------------------------------------------------------- status

type rtTopicInfo struct {
	Topic    string `json:"topic"`
	Clients  int    `json:"clients"`
	Presence int    `json:"presence"`
	PGSubs   int    `json:"postgres_changes"`
}

func (p *rtProject) status() map[string]any {
	p.mu.Lock()
	var topics []rtTopicInfo
	for t, set := range p.topics {
		ti := rtTopicInfo{Topic: strings.TrimPrefix(t, "realtime:"), Clients: len(set)}
		for ch := range set {
			if ch.pres != nil {
				ti.Presence++
			}
			ti.PGSubs += len(ch.pg)
		}
		topics = append(topics, ti)
	}
	var conns []map[string]any
	for c := range p.conns {
		var chs []string
		roles := map[string]bool{}
		for t, ch := range c.chans {
			chs = append(chs, strings.TrimPrefix(t, "realtime:"))
			roles[ch.role] = true
		}
		sort.Strings(chs)
		var rl []string
		for r := range roles {
			rl = append(rl, r)
		}
		sort.Strings(rl)
		conns = append(conns, map[string]any{"id": c.id, "ip": c.ip, "user_agent": c.ua, "connected_at": c.started,
			"channels": chs, "roles": rl})
	}
	n := len(p.conns)
	p.mu.Unlock()
	sort.Slice(topics, func(i, j int) bool { return topics[i].Topic < topics[j].Topic })
	sort.Slice(conns, func(i, j int) bool {
		return conns[i]["connected_at"].(time.Time).Before(conns[j]["connected_at"].(time.Time))
	})
	if topics == nil {
		topics = []rtTopicInfo{}
	}
	if conns == nil {
		conns = []map[string]any{}
	}
	return map[string]any{
		"connections": n, "max_connections": rtMaxConnsPerProject, "channels": topics, "clients": conns,
		"messages_in": p.msgsIn.Load(), "messages_out": p.msgsOut.Load(), "dropped": p.dropped.Load(),
		"postgres_changes": p.pipelineStatus(),
	}
}

func logRealtime(format string, args ...any) { log.Printf("realtime: "+format, args...) }
