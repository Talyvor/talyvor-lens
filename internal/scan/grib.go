package scan

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// gribField is one GRIB2 message on a regular latitude/longitude grid (grid template 3.0), decoded.
type gribField struct {
	ni, nj             int
	lat0, lon0, di, dj float64 // degrees; rows run north to south, columns west to east (scanning mode 0)
	values             []float64
}

var errGrib = errors.New("scan: unsupported GRIB2 message")

// decodeGrib reads a single GRIB2 message: grid template 3.0, scanning mode 0, no bitmap, and data packed with
// template 5.0 (simple) or 5.42 (CCSDS, which ECMWF's open data uses).
func decodeGrib(b []byte) (*gribField, error) {
	if len(b) < 16 || string(b[:4]) != "GRIB" || b[7] != 2 {
		return nil, fmt.Errorf("%w: not a GRIB2 message", errGrib)
	}
	f := &gribField{}
	var tmpl5, npts int
	var sec5, sec7 []byte
	for p := 16; p+5 <= len(b) && string(b[p:p+4]) != "7777"; {
		n := int(binary.BigEndian.Uint32(b[p:]))
		if n < 5 || p+n > len(b) {
			return nil, fmt.Errorf("%w: truncated section", errGrib)
		}
		sec := b[p : p+n]
		switch sec[4] {
		case 3:
			if n < 72 || binary.BigEndian.Uint16(sec[12:]) != 0 || sec[71] != 0 {
				return nil, fmt.Errorf("%w: grid is not a regular lat/lon grid scanned in mode 0", errGrib)
			}
			f.ni, f.nj = int(binary.BigEndian.Uint32(sec[30:])), int(binary.BigEndian.Uint32(sec[34:]))
			f.lat0, f.lon0 = microDeg(sec[46:]), microDeg(sec[50:])
			f.di, f.dj = microDeg(sec[63:]), microDeg(sec[67:])
		case 5:
			if n < 21 {
				return nil, fmt.Errorf("%w: short data representation", errGrib)
			}
			npts, tmpl5, sec5 = int(binary.BigEndian.Uint32(sec[5:])), int(binary.BigEndian.Uint16(sec[9:])), sec
		case 6:
			if sec[5] != 255 {
				return nil, fmt.Errorf("%w: bitmap present", errGrib)
			}
		case 7:
			sec7 = sec[5:]
		}
		p += n
	}
	if sec5 == nil || sec7 == nil || f.ni == 0 || npts != f.ni*f.nj {
		return nil, fmt.Errorf("%w: missing grid, data representation or data", errGrib)
	}
	ref := float64(math.Float32frombits(binary.BigEndian.Uint32(sec5[11:])))
	e, d, bits := signMag16(sec5[15:]), signMag16(sec5[17:]), int(sec5[19])
	var raw []uint32
	var err error
	switch {
	case bits == 0:
		raw = make([]uint32, npts)
	case tmpl5 == 0:
		raw, err = unpackSimple(sec7, npts, bits)
	case tmpl5 == 42 && len(sec5) >= 25:
		raw, err = unpackCCSDS(sec7, npts, bits, sec5[21], int(sec5[22]), int(binary.BigEndian.Uint16(sec5[23:])))
	default:
		return nil, fmt.Errorf("%w: data template 5.%d", errGrib, tmpl5)
	}
	if err != nil {
		return nil, err
	}
	scale, dec := math.Pow(2, float64(e)), math.Pow(10, float64(d))
	f.values = make([]float64, npts)
	for i, x := range raw {
		f.values[i] = (ref + float64(x)*scale) / dec
	}
	return f, nil
}

// nearest is the value at the grid point closest to lat, lon, and that point.
func (f *gribField) nearest(lat, lon float64) (v, glat, glon float64) {
	j := int(math.Round((f.lat0 - lat) / f.dj))
	i := int(math.Round(math.Mod(lon-f.lon0+720, 360) / f.di))
	j, i = min(max(j, 0), f.nj-1), i%f.ni
	glon = math.Mod(f.lon0+float64(i)*f.di+540, 360) - 180
	return f.values[j*f.ni+i], f.lat0 - float64(j)*f.dj, glon
}

func microDeg(b []byte) float64 { return float64(signMag32(b)) / 1e6 }

func signMag32(b []byte) int64 {
	v := binary.BigEndian.Uint32(b)
	if v&0x80000000 != 0 {
		return -int64(v & 0x7fffffff)
	}
	return int64(v)
}

func signMag16(b []byte) int {
	v := binary.BigEndian.Uint16(b)
	if v&0x8000 != 0 {
		return -int(v & 0x7fff)
	}
	return int(v)
}

type bitReader struct {
	b   []byte
	pos int // in bits
}

var errShort = fmt.Errorf("%w: data ends early", errGrib)

func (r *bitReader) bits(n int) (uint32, error) {
	if r.pos+n > len(r.b)*8 {
		return 0, errShort
	}
	var v uint32
	for k := 0; k < n; k++ {
		v = v<<1 | uint32(r.b[r.pos>>3]>>(7-r.pos&7))&1
		r.pos++
	}
	return v, nil
}

// unary reads a fundamental sequence: the count of 0 bits before the next 1.
func (r *bitReader) unary() (int, error) {
	n := 0
	for {
		if r.pos >= len(r.b)*8 {
			return 0, errShort
		}
		if r.b[r.pos>>3]>>(7-r.pos&7)&1 == 1 {
			r.pos++
			return n, nil
		}
		r.pos++
		n++
	}
}

func unpackSimple(b []byte, n, bits int) ([]uint32, error) {
	r := &bitReader{b: b}
	out := make([]uint32, n)
	for i := range out {
		v, err := r.bits(bits)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// unpackCCSDS decodes CCSDS 121.0-B lossless data (adaptive Rice coding, as libaec writes it for GRIB2 template
// 5.42): unsigned samples of `bits` bits, blocks of `block` samples, a reference sample every `rsi` blocks when the
// preprocessor is on. Signed data, restricted codes and padded intervals are refused.
func unpackCCSDS(b []byte, n, bits int, flags byte, block, rsi int) ([]uint32, error) {
	const flagSigned, flagPreprocess, flagRestricted, flagPadRSI = 1, 8, 16, 32
	if flags&(flagSigned|flagRestricted|flagPadRSI) != 0 || block <= 0 || rsi <= 0 || bits > 32 {
		return nil, fmt.Errorf("%w: CCSDS flags %d", errGrib, flags)
	}
	pp := flags&flagPreprocess != 0
	idLen := 3
	switch {
	case bits > 16:
		idLen = 5
	case bits > 8:
		idLen = 4
	}
	xmax := uint64(1)<<bits - 1
	r := &bitReader{b: b}
	out := make([]uint32, 0, n)
	residual := make([]uint32, 0, block)
	blockInRSI := 0
	var last uint64
	for len(out) < n {
		ref := pp && blockInRSI == 0
		id, err := r.bits(idLen)
		if err != nil {
			return nil, err
		}
		residual = residual[:0]
		blocks := 1
		uncompressed := int(id) == 1<<idLen-1
		var se uint32
		if id == 0 { // low entropy: one bit picks the second extension or a run of zero blocks, before the reference
			if se, err = r.bits(1); err != nil {
				return nil, err
			}
		}
		start := 0
		if ref && !uncompressed { // an uncompressed block carries its reference as its first raw sample instead
			v, err := r.bits(bits)
			if err != nil {
				return nil, err
			}
			last, start = uint64(v), 1
			out = append(out, v)
		}
		switch {
		case id == 0:
			if se == 1 { // second extension: pairs coded together
				for i := start; i < block; {
					m, err := r.unary()
					if err != nil {
						return nil, err
					}
					beta := int((math.Sqrt(float64(8*m+1)) - 1) / 2)
					d1 := uint32(m - beta*(beta+1)/2)
					if i&1 == 0 {
						residual = append(residual, uint32(beta)-d1)
						i++
					}
					residual = append(residual, d1)
					i++
				}
				break
			}
			z, err := r.unary()
			if err != nil {
				return nil, err
			}
			blocks = z + 1
			if blocks == 5 { // "remainder of segment": to the end of the reference interval or of the 64-block segment
				blocks = min(rsi-blockInRSI, 64-blockInRSI%64)
			} else if blocks > 5 {
				blocks--
			}
			residual = append(residual, make([]uint32, blocks*block-start)...)
		case uncompressed:
			for i := 0; i < block; i++ {
				v, err := r.bits(bits)
				if err != nil {
					return nil, err
				}
				residual = append(residual, v)
			}
		default:
			k := int(id) - 1
			for i := start; i < block; i++ {
				m, err := r.unary()
				if err != nil {
					return nil, err
				}
				residual = append(residual, uint32(m)<<k)
			}
			for i := range residual {
				v, err := r.bits(k)
				if err != nil {
					return nil, err
				}
				residual[i] |= v
			}
		}
		if ref && uncompressed {
			last = uint64(residual[0])
			out = append(out, residual[0])
			residual = residual[1:]
		}
		for _, d := range residual {
			if !pp {
				out = append(out, d)
				continue
			}
			// The preprocessor's inverse: a unit-delay predictor, and the residual mapped back to its sign.
			theta := min(last, xmax-last)
			dd := uint64(d)
			switch {
			case dd <= 2*theta && dd&1 == 0:
				last += dd / 2
			case dd <= 2*theta:
				last -= (dd + 1) / 2
			case theta == last:
				last += dd - theta
			default:
				last -= dd - theta
			}
			out = append(out, uint32(last))
		}
		blockInRSI = (blockInRSI + blocks) % rsi
	}
	return out[:n], nil
}
