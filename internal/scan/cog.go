package scan

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// A minimal reader for the Cloud-Optimised GeoTIFFs Sentinel-2 scenes are published as: classic little-endian TIFF,
// single band, 8- or 16-bit unsigned, tiled, Deflate or Zstandard (Sentinel-1 GRD) with or without horizontal
// differencing. It reads one overview
// level tile by tile over HTTP ranges, so a regional reading fetches a few hundred kilobytes, not a 200 MB scene.

var errCOG = errors.New("scan: unsupported GeoTIFF")

// rangeReader fetches n bytes at off, returning fewer only at the end of the file.
type rangeReader func(ctx context.Context, off, n int64) ([]byte, error)

type cogLevel struct {
	width, height, tileW, tileH, bits, predictor int
	compression                                  int
	offsets, counts                              []int64
}

type cogFile struct {
	read   rangeReader
	levels []cogLevel // full resolution first, then each overview
}

// cogHeaderBytes covers every IFD of a COG, which keeps them all before the first tile.
const cogHeaderBytes = 64 << 10

func openCOG(ctx context.Context, read rangeReader) (*cogFile, error) {
	head, err := read(ctx, 0, cogHeaderBytes)
	if err != nil {
		return nil, err
	}
	if len(head) < 8 || string(head[:2]) != "II" || binary.LittleEndian.Uint16(head[2:]) != 42 {
		return nil, fmt.Errorf("%w: not a little-endian classic TIFF", errCOG)
	}
	le := binary.LittleEndian
	at := func(off, n int64) ([]byte, error) {
		if off+n <= int64(len(head)) {
			return head[off : off+n], nil
		}
		b, err := read(ctx, off, n)
		if err == nil && int64(len(b)) < n {
			err = fmt.Errorf("%w: short read at %d", errCOG, off)
		}
		return b, err
	}
	f := &cogFile{read: read}
	for ifd := int64(le.Uint32(head[4:])); ifd != 0 && len(f.levels) < 16; {
		nb, err := at(ifd, 2)
		if err != nil {
			return nil, err
		}
		n := int64(le.Uint16(nb))
		entries, err := at(ifd+2, n*12+4)
		if err != nil {
			return nil, err
		}
		lv := cogLevel{predictor: 1, compression: 1}
		for i := int64(0); i < n; i++ {
			e := entries[i*12:]
			tag, typ, count := le.Uint16(e), le.Uint16(e[2:]), int64(le.Uint32(e[4:]))
			vals, err := tiffValues(at, typ, count, e[8:12])
			if err != nil {
				return nil, err
			}
			one := func() int {
				if len(vals) == 0 {
					return 0
				}
				return int(vals[0])
			}
			switch tag {
			case 256:
				lv.width = one()
			case 257:
				lv.height = one()
			case 258:
				lv.bits = one()
			case 259:
				lv.compression = one()
			case 277:
				if one() != 1 {
					return nil, fmt.Errorf("%w: %d samples per pixel", errCOG, one())
				}
			case 317:
				lv.predictor = one()
			case 322:
				lv.tileW = one()
			case 323:
				lv.tileH = one()
			case 324:
				lv.offsets = vals
			case 325:
				lv.counts = vals
			}
		}
		if lv.tileW == 0 || lv.tileH == 0 || len(lv.offsets) != len(lv.counts) ||
			len(lv.offsets) != ceilDiv(lv.width, lv.tileW)*ceilDiv(lv.height, lv.tileH) {
			return nil, fmt.Errorf("%w: not tiled as a COG", errCOG)
		}
		// Bounds well above Sentinel-2's (10980 px, 1024 px tiles, a few MB a tile), so a bad header cannot make
		// the reader allocate or fetch without limit.
		if lv.width <= 0 || lv.height <= 0 || lv.width > 1<<15 || lv.height > 1<<15 || lv.tileW > 4096 || lv.tileH > 4096 {
			return nil, fmt.Errorf("%w: %d×%d px in %d×%d tiles", errCOG, lv.width, lv.height, lv.tileW, lv.tileH)
		}
		if lv.bits != 8 && lv.bits != 16 {
			return nil, fmt.Errorf("%w: %d bits per sample", errCOG, lv.bits)
		}
		// Deflate never stores a tile in more than its raw size plus a few bytes per 64 KB block.
		maxTile := int64(lv.tileW*lv.tileH*lv.bits/8) + 4096
		for _, n := range lv.counts {
			if n < 0 || n > maxTile {
				return nil, fmt.Errorf("%w: a %d-byte tile", errCOG, n)
			}
		}
		if lv.compression != 1 && lv.compression != 8 && lv.compression != 32946 && lv.compression != 50000 {
			return nil, fmt.Errorf("%w: compression %d", errCOG, lv.compression)
		}
		if lv.predictor != 1 && lv.predictor != 2 {
			return nil, fmt.Errorf("%w: predictor %d", errCOG, lv.predictor)
		}
		f.levels = append(f.levels, lv)
		ifd = int64(le.Uint32(entries[n*12:]))
	}
	if len(f.levels) == 0 {
		return nil, fmt.Errorf("%w: no image", errCOG)
	}
	return f, nil
}

// tiffValues reads an IFD entry's values: SHORT or LONG, inline when they fit in four bytes.
func tiffValues(at func(off, n int64) ([]byte, error), typ uint16, count int64, inline []byte) ([]int64, error) {
	size := map[uint16]int64{3: 2, 4: 4}[typ]
	if size == 0 || count > 1<<16 {
		return nil, nil // a tag this reader does not use
	}
	b := inline
	if size*count > 4 {
		var err error
		if b, err = at(int64(binary.LittleEndian.Uint32(inline)), size*count); err != nil {
			return nil, err
		}
	}
	out := make([]int64, count)
	for i := range out {
		if size == 2 {
			out[i] = int64(binary.LittleEndian.Uint16(b[i*2:]))
		} else {
			out[i] = int64(binary.LittleEndian.Uint32(b[i*4:]))
		}
	}
	return out, nil
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// level returns the level whose width is nearest to width.
func (f *cogFile) level(width int) (int, cogLevel) {
	best := 0
	for i, lv := range f.levels {
		if abs(lv.width-width) < abs(f.levels[best].width-width) {
			best = i
		}
	}
	return best, f.levels[best]
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// window reads the pixels [x0,x1)×[y0,y1) of a level, row by row, and every compressed byte it fetched (for the
// content hash).
func (f *cogFile) window(ctx context.Context, level, x0, y0, x1, y1 int) ([]uint16, []byte, error) {
	lv := f.levels[level]
	if x0 < 0 || y0 < 0 || x1 > lv.width || y1 > lv.height || x0 >= x1 || y0 >= y1 {
		return nil, nil, fmt.Errorf("%w: window outside the image", errCOG)
	}
	w := x1 - x0
	out := make([]uint16, w*(y1-y0))
	var raw bytes.Buffer
	across := ceilDiv(lv.width, lv.tileW)
	for ty := y0 / lv.tileH; ty <= (y1-1)/lv.tileH; ty++ {
		for tx := x0 / lv.tileW; tx <= (x1-1)/lv.tileW; tx++ {
			i := ty*across + tx
			var px []uint16
			if lv.counts[i] > 0 { // a tile of only nodata may be left out of the file
				b, err := f.read(ctx, lv.offsets[i], lv.counts[i])
				if err != nil {
					return nil, nil, err
				}
				raw.Write(b)
				if px, err = lv.decode(b); err != nil {
					return nil, nil, err
				}
			} else {
				px = make([]uint16, lv.tileW*lv.tileH)
			}
			for y := max(y0, ty*lv.tileH); y < min(y1, (ty+1)*lv.tileH); y++ {
				for x := max(x0, tx*lv.tileW); x < min(x1, (tx+1)*lv.tileW); x++ {
					out[(y-y0)*w+(x-x0)] = px[(y-ty*lv.tileH)*lv.tileW+(x-tx*lv.tileW)]
				}
			}
		}
	}
	return out, raw.Bytes(), nil
}

// zstdDecoder decodes whole tiles; its memory is bounded by the tile size checked in openCOG.
var zstdDecoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(64<<20))

func (lv cogLevel) decode(b []byte) ([]uint16, error) {
	want := lv.tileW * lv.tileH * lv.bits / 8
	if lv.compression == 50000 {
		var err error
		if b, err = zstdDecoder.DecodeAll(b, make([]byte, 0, want)); err != nil {
			return nil, fmt.Errorf("%w: %v", errCOG, err)
		}
	} else if lv.compression != 1 {
		z, err := zlib.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errCOG, err)
		}
		if b, err = io.ReadAll(io.LimitReader(z, int64(want)+1)); err != nil {
			return nil, fmt.Errorf("%w: %v", errCOG, err)
		}
	}
	if len(b) != want {
		return nil, fmt.Errorf("%w: tile has %d bytes, want %d", errCOG, len(b), want)
	}
	px := make([]uint16, lv.tileW*lv.tileH)
	for i := range px {
		if lv.bits == 16 {
			px[i] = binary.LittleEndian.Uint16(b[i*2:])
		} else {
			px[i] = uint16(b[i])
		}
	}
	if lv.predictor == 2 {
		mask := uint16(1<<lv.bits - 1)
		for y := 0; y < lv.tileH; y++ {
			row := px[y*lv.tileW : (y+1)*lv.tileW]
			for x := 1; x < len(row); x++ {
				row[x] = (row[x] + row[x-1]) & mask
			}
		}
	}
	return px, nil
}
