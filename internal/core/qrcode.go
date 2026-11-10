package core

import (
	"errors"
	"fmt"
	"strings"
)

// A small QR code encoder (byte mode, error correction level M, versions
// 1-20: up to 666 bytes) for TOTP enrollment, where supabase-js expects an SVG
// `qr_code`. Follows ISO/IEC 18004; structure after Project Nayuki's
// reference implementation.

type qrBlocks struct{ ecPerBlock, g1Blocks, g1Data, g2Blocks, g2Data int }

// level M block structure per version
var qrM = [21]qrBlocks{{},
	{10, 1, 16, 0, 0}, {16, 1, 28, 0, 0}, {26, 1, 44, 0, 0}, {18, 2, 32, 0, 0}, {24, 2, 43, 0, 0},
	{16, 4, 27, 0, 0}, {18, 4, 31, 0, 0}, {22, 2, 38, 2, 39}, {22, 3, 36, 2, 37}, {26, 4, 43, 1, 44},
	{30, 1, 50, 4, 51}, {22, 6, 36, 2, 37}, {22, 8, 37, 1, 38}, {24, 4, 40, 5, 41}, {24, 5, 41, 5, 42},
	{28, 7, 45, 3, 46}, {28, 10, 46, 1, 47}, {26, 9, 43, 4, 44}, {26, 3, 44, 11, 45}, {26, 3, 41, 13, 42},
}

var qrAlign = [21][]int{{}, {},
	{6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
	{6, 30, 54}, {6, 32, 58}, {6, 34, 62}, {6, 26, 46, 66}, {6, 26, 48, 70}, {6, 26, 50, 74},
	{6, 30, 54, 78}, {6, 30, 56, 82}, {6, 30, 58, 86}, {6, 34, 62, 90},
}

func (b qrBlocks) dataCodewords() int { return b.g1Blocks*b.g1Data + b.g2Blocks*b.g2Data }

type qrCode struct {
	size    int
	mod     [][]bool
	isFunc  [][]bool
	version int
}

// qrEncode returns the module matrix (true = dark) for text.
func qrEncode(text string) ([][]bool, error) {
	data := []byte(text)
	ver := 0
	for v := 1; v <= 20; v++ {
		ccBits := 8
		if v >= 10 {
			ccBits = 16
		}
		if 4+ccBits+8*len(data) <= qrM[v].dataCodewords()*8 {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, errors.New("qr: text too long")
	}
	// bit stream: mode 0100, count, bytes, terminator, padding
	var bits []bool
	put := func(val, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (val>>i)&1 == 1)
		}
	}
	put(4, 4)
	if ver >= 10 {
		put(len(data), 16)
	} else {
		put(len(data), 8)
	}
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := qrM[ver].dataCodewords() * 8
	for i := 0; i < 4 && len(bits) < capBits; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	cw := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			cw[i/8] |= 1 << (7 - i%8)
		}
	}

	q := &qrCode{version: ver, size: ver*4 + 17}
	q.mod = make([][]bool, q.size)
	q.isFunc = make([][]bool, q.size)
	for i := range q.mod {
		q.mod[i] = make([]bool, q.size)
		q.isFunc[i] = make([]bool, q.size)
	}
	q.drawFunctionPatterns()
	q.drawCodewords(qrInterleave(cw, qrM[ver]))

	best, bestPenalty := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormat(m)
		if p := q.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = m, p
		}
		q.applyMask(m) // undo (XOR)
	}
	q.applyMask(best)
	q.drawFormat(best)
	return q.mod, nil
}

func (q *qrCode) set(x, y int, dark bool) {
	q.mod[y][x] = dark
	q.isFunc[y][x] = true
}

func (q *qrCode) drawFunctionPatterns() {
	for i := 0; i < q.size; i++ {
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {q.size - 4, 3}, {3, q.size - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := c[0]+dx, c[1]+dy
				if x >= 0 && x < q.size && y >= 0 && y < q.size {
					d := max(abs(dx), abs(dy))
					q.set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	pos := qrAlign[q.version]
	n := len(pos)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			if i == 0 && j == 0 || i == 0 && j == n-1 || i == n-1 && j == 0 {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.set(pos[i]+dx, pos[j]+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reserve; redrawn per mask
	if q.version >= 7 {
		rem := q.version
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		b := q.version<<12 | rem
		for i := 0; i < 18; i++ {
			dark := (b>>i)&1 == 1
			a, c := q.size-11+i%3, i/3
			q.set(a, c, dark)
			q.set(c, a, dark)
		}
	}
}

func (q *qrCode) drawFormat(mask int) {
	data := 0<<3 | mask // level M = 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	b := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (b>>i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(q.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, q.size-15+i, bit(i))
	}
	q.set(8, q.size-8, true)
}

func (q *qrCode) drawCodewords(data []byte) {
	i := 0
	for right := q.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.size - 1 - vert
				}
				if !q.isFunc[y][x] && i < len(data)*8 {
					q.mod[y][x] = (data[i>>3]>>(7-uint(i&7)))&1 == 1
					i++
				}
			}
		}
	}
}

func (q *qrCode) applyMask(m int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv && !q.isFunc[y][x] {
				q.mod[y][x] = !q.mod[y][x]
			}
		}
	}
}

// penalty scores a masked symbol (lower is better); only affects which mask
// is chosen, never whether the code scans.
func (q *qrCode) penalty() int {
	n, p, dark := q.size, 0, 0
	line := func(get func(i int) bool) {
		run := 1
		for i := 1; i < n; i++ {
			if get(i) == get(i-1) {
				run++
				if run == 5 {
					p += 3
				} else if run > 5 {
					p++
				}
			} else {
				run = 1
			}
		}
		for i := 0; i+11 <= n; i++ {
			a, b := true, true
			pat := []bool{true, false, true, true, true, false, true, false, false, false, false}
			for k := 0; k < 11; k++ {
				if get(i+k) != pat[k] {
					a = false
				}
				if get(i+k) != pat[10-k] {
					b = false
				}
			}
			if a || b {
				p += 40
			}
		}
	}
	for y := 0; y < n; y++ {
		yy := y
		line(func(i int) bool { return q.mod[yy][i] })
	}
	for x := 0; x < n; x++ {
		xx := x
		line(func(i int) bool { return q.mod[i][xx] })
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.mod[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				c := q.mod[y][x]
				if q.mod[y][x+1] == c && q.mod[y+1][x] == c && q.mod[y+1][x+1] == c {
					p += 3
				}
			}
		}
	}
	pct := dark * 100 / (n * n)
	p += abs(pct-50) / 5 * 10
	return p
}

func qrInterleave(data []byte, b qrBlocks) []byte {
	var blocks, ecs [][]byte
	off := 0
	gen := rsDivisor(b.ecPerBlock)
	for i := 0; i < b.g1Blocks+b.g2Blocks; i++ {
		n := b.g1Data
		if i >= b.g1Blocks {
			n = b.g2Data
		}
		blk := data[off : off+n]
		off += n
		blocks = append(blocks, blk)
		ecs = append(ecs, rsRemainder(blk, gen))
	}
	var out []byte
	for i := 0; i < b.g2Data || i < b.g1Data; i++ {
		for _, blk := range blocks {
			if i < len(blk) {
				out = append(out, blk[i])
			}
		}
	}
	for i := 0; i < b.ecPerBlock; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out
}

func gfMul(x, y byte) byte {
	var z int
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= int((y>>uint(i))&1) * int(x)
	}
	return byte(z)
}

func rsDivisor(degree int) []byte {
	r := make([]byte, degree)
	r[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			r[j] = gfMul(r[j], root)
			if j+1 < degree {
				r[j] ^= r[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return r
}

func rsRemainder(data, div []byte) []byte {
	res := make([]byte, len(div))
	for _, b := range data {
		f := b ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for i := range res {
			res[i] ^= gfMul(div[i], f)
		}
	}
	return res
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// qrSVG renders text as a compact SVG (4-module quiet zone). No '#' anywhere:
// supabase-js puts it into a data: URI without escaping.
func qrSVG(text string) (string, error) {
	m, err := qrEncode(text)
	if err != nil {
		return "", err
	}
	n := len(m) + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges">`, n, n, n*5, n*5)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="white"/><path fill="black" d="`, n, n)
	for y, row := range m {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+4, y+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
