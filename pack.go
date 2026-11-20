package main

// Packfile codec: deterministic version-2 pack encode (no deltas) and a
// full decoder that also resolves OFS_DELTA / REF_DELTA entries.
//
// Wire note (verified against program/env/src/git-client.ts, which is the
// source of truth): a pack entry's zlib stream holds the object's RAW bytes
// (no "type len\0" loose header); the object id is still SHA-1 over
// "type len\0" + raw bytes. The client's writePack/readPack agree on this.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

// packTypeCode maps an object type to its pack type code.
func packTypeCode(typ string) int {
	switch typ {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 0
}

// packTypeName is the inverse of packTypeCode ("" for delta/unknown codes).
func packTypeName(code int) string {
	switch code {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

// packObjectHeader encodes one entry's type/size varint.
func packObjectHeader(typeCode, size int) []byte {
	var out []byte
	first := byte((typeCode << 4) | (size & 0x0f))
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, first)
	return out
}

// encodePack builds a deterministic packfile (version 2, no deltas).
// Entries are ordered commit, tree, blob, tag; each group sorted by oid, so
// the same object set always produces byte-identical output.
func encodePack(objects map[string]*GitObject) []byte {
	type entry struct {
		typ  string
		oid  string
		data []byte
	}
	list := make([]entry, 0, len(objects))
	for oid, obj := range objects {
		list = append(list, entry{obj.Type, oid, obj.Data})
	}
	sort.Slice(list, func(i, j int) bool {
		ri, rj := packTypeCode(list[i].typ), packTypeCode(list[j].typ)
		if ri != rj {
			return ri < rj
		}
		return list[i].oid < list[j].oid
	})
	var body bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(list)))
	body.Write(hdr)
	for _, e := range list {
		body.Write(packObjectHeader(packTypeCode(e.typ), len(e.data)))
		zw, _ := zlib.NewWriterLevel(&body, zlib.DefaultCompression)
		_, _ = zw.Write(e.data)
		_ = zw.Close()
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

// buildFullPack builds the cached full pack: every object reachable from
// every ref, in deterministic order.
func buildFullPack(g *GitRepo) []byte {
	var roots []string
	for _, name := range sortedRefNames(g) {
		roots = append(roots, g.Refs[name])
	}
	reachable := g.collectReachable(roots)
	objects := make(map[string]*GitObject, len(reachable))
	for oid := range reachable {
		if obj := g.Objects[oid]; obj != nil {
			objects[oid] = obj
		}
	}
	return encodePack(objects)
}

// packEntry is one decoded pack object (delta-resolved).
type packEntry struct {
	typ  string
	data []byte
	oid  string
}

// decodePack parses a packfile (version 2/3), inflates every entry and
// resolves OFS_DELTA / REF_DELTA entries. resolveBase supplies objects a
// thin pack leaves out (objects already in the store); it may be nil.
func decodePack(pack []byte, resolveBase func(string) *GitObject) ([]packEntry, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack is too short")
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	want := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(want[:], pack[trailerStart:]) {
		return nil, errors.New("pack trailer checksum mismatch")
	}

	type raw struct {
		offset   int
		typeCode int
		size     int
		data     []byte
		baseOff  int // ofs-delta: absolute offset of the base entry
		baseOID  string
		resolved bool
		resTyp   string
		resData  []byte
		resOID   string
	}
	raws := make([]*raw, 0, count)
	byOffset := make(map[int]*raw, count)
	off := 12
	for i := 0; i < count; i++ {
		start := off
		if off >= trailerStart {
			return nil, errors.New("pack is truncated")
		}
		b := pack[off]
		off++
		typeCode := int((b >> 4) & 0x07)
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if off >= trailerStart {
				return nil, errors.New("pack entry size varint is truncated")
			}
			b = pack[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
			if shift > 63 {
				return nil, errors.New("pack entry size varint is too long")
			}
		}
		e := &raw{offset: start, typeCode: typeCode, size: size}
		if typeCode == 6 {
			if off >= trailerStart {
				return nil, errors.New("ofs-delta has no base offset")
			}
			b = pack[off]
			off++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				if off >= trailerStart {
					return nil, errors.New("ofs-delta base offset is truncated")
				}
				b = pack[off]
				off++
				v = ((v + 1) << 7) | int(b&0x7f)
			}
			e.baseOff = start - v
			if e.baseOff < 0 {
				return nil, errors.New("ofs-delta base offset is negative")
			}
		} else if typeCode == 7 {
			if off+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if packTypeName(typeCode) == "" {
			return nil, fmt.Errorf("pack entry has unknown type code %d", typeCode)
		}
		// Inflate exactly one zlib stream starting at off. Go's zlib reader
		// consumes precisely the stream's bytes (verified), so the bytes
		// left in the reader tell us where the next entry starts.
		br := bytes.NewReader(pack[off:trailerStart])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("pack entry at offset %d: %v", start, err)
		}
		data, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, fmt.Errorf("pack entry at offset %d did not inflate: %v", start, err)
		}
		consumed := (trailerStart - off) - br.Len()
		off += consumed
		if len(data) != size {
			return nil, fmt.Errorf("pack entry at offset %d declares %d bytes but inflated to %d", start, size, len(data))
		}
		e.data = data
		raws = append(raws, e)
		byOffset[start] = e
	}
	if off != trailerStart {
		return nil, errors.New("pack has stray bytes before its trailer")
	}

	resolvedByOID := make(map[string]*raw)
	resolving := make(map[int]bool)
	var resolve func(e *raw) (string, []byte, error)
	resolve = func(e *raw) (string, []byte, error) {
		if e.resolved {
			return e.resTyp, e.resData, nil
		}
		if resolving[e.offset] {
			return "", nil, errors.New("delta cycle in pack")
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			base := byOffset[e.baseOff]
			if base == nil {
				return "", nil, fmt.Errorf("ofs-delta base at offset %d is not in the pack", e.baseOff)
			}
			btyp, bdata, err := resolve(base)
			if err != nil {
				return "", nil, err
			}
			typ = btyp
			data, err = applyDelta(bdata, e.data)
			if err != nil {
				return "", nil, err
			}
		case 7:
			if base, ok := resolvedByOID[e.baseOID]; ok {
				btyp, bdata, err := resolve(base)
				if err != nil {
					return "", nil, err
				}
				typ = btyp
				data, err = applyDelta(bdata, e.data)
				if err != nil {
					return "", nil, err
				}
			} else if resolveBase != nil {
				ext := resolveBase(e.baseOID)
				if ext == nil {
					return "", nil, fmt.Errorf("ref-delta base %s is not in the pack or the store", e.baseOID)
				}
				var err error
				typ = ext.Type
				data, err = applyDelta(ext.Data, e.data)
				if err != nil {
					return "", nil, err
				}
			} else {
				return "", nil, fmt.Errorf("ref-delta base %s is not in the pack", e.baseOID)
			}
		default:
			typ = packTypeName(e.typeCode)
			data = e.data
		}
		oid := sha1Hex(typ, data)
		e.resolved = true
		e.resTyp = typ
		e.resData = data
		e.resOID = oid
		resolvedByOID[oid] = e
		return typ, data, nil
	}
	// Resolve plain entries first so a REF_DELTA whose base sits later in
	// the pack still finds it.
	for _, e := range raws {
		if e.typeCode != 6 && e.typeCode != 7 {
			if _, _, err := resolve(e); err != nil {
				return nil, err
			}
		}
	}
	out := make([]packEntry, 0, len(raws))
	for _, e := range raws {
		typ, data, err := resolve(e)
		if err != nil {
			return nil, err
		}
		out = append(out, packEntry{typ: typ, data: data, oid: e.resOID})
	}
	return out, nil
}

// applyDelta applies a git delta instruction stream (copy/insert) to a base
// object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readVarint := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[off]
			off++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, errors.New("delta size varint is too long")
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", srcSize, len(base))
	}
	out := make([]byte, 0, dstSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOff |= int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize |= int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) {
				return nil, errors.New("delta copy is out of the base")
			}
			out = append(out, base[copyOff:copyOff+copySize]...)
		} else if op != 0 {
			n := int(op & 0x7f)
			if off+n > len(delta) {
				return nil, errors.New("delta insert is truncated")
			}
			out = append(out, delta[off:off+n]...)
			off += n
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if len(out) != dstSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", len(out), dstSize)
	}
	return out, nil
}
