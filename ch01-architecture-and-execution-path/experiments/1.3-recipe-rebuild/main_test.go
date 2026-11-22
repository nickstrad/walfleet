package main

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"
)

func newImage(pages int) []byte {
	data := make([]byte, pages*pageSize)
	binary.BigEndian.PutUint16(data[16:], pageSize)
	data[100] = 0x0d
	binary.BigEndian.PutUint16(data[105:], pageSize)
	return data
}

func setFreelist(data []byte, firstTrunk, total int) {
	binary.BigEndian.PutUint32(data[32:], uint32(firstTrunk))
	binary.BigEndian.PutUint32(data[36:], uint32(total))
}

func setTrunk(data []byte, page, next int, leaves ...int) {
	base := (page - 1) * pageSize
	binary.BigEndian.PutUint32(data[base:], uint32(next))
	binary.BigEndian.PutUint32(data[base+4:], uint32(len(leaves)))
	for i, leaf := range leaves {
		binary.BigEndian.PutUint32(data[base+8+4*i:], uint32(leaf))
	}
}

// TestFreelistTrunkMetadataIsNotUnused checks that a freelist trunk page's
// header and leaf-pointer array are treated as live metadata while bytes after
// the leaf list and all bytes of a freelist leaf are marked unused.
func TestFreelistTrunkMetadataIsNotUnused(t *testing.T) {
	data := newImage(4)
	setFreelist(data, 3, 2)
	setTrunk(data, 3, 0, 4)
	fl, err := readFreelist(data)
	if err != nil {
		t.Fatal(err)
	}
	unused, kind, err := unusedBytes(data, 3, fl)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kind, "trunk") {
		t.Fatalf("kind %q", kind)
	}
	for i := range 12 {
		if unused[i] {
			t.Fatalf("trunk byte %d is metadata but marked unused", i)
		}
	}
	if !unused[12] || !unused[pageSize-1] {
		t.Fatal("trunk bytes after the leaf list should be unused")
	}
	leafUnused, leafKind, err := unusedBytes(data, 4, fl)
	if err != nil || !strings.Contains(leafKind, "leaf") || slices.Contains(leafUnused, false) {
		t.Fatalf("leaf page: kind %q err %v", leafKind, err)
	}

	other := slices.Clone(data)
	binary.BigEndian.PutUint32(other[2*pageSize+8:], 2)
	d := diffImages(data, other)
	report := classifyImages(data, other, d)
	if report.OnlyUnused {
		t.Fatal("a differing leaf pointer on a trunk page was classified as unused")
	}
}

// TestFreelistRejectsMalformedChains checks that readFreelist rejects
// out-of-range trunks or leaves, overflowing leaf counts, trunk cycles,
// duplicate entries, mismatched header totals, partial pages, and wrong page
// sizes.
func TestFreelistRejectsMalformedChains(t *testing.T) {
	cases := map[string]func([]byte){
		"trunk past the end": func(d []byte) { setFreelist(d, 9, 1) },
		"too many leaves":    func(d []byte) { setFreelist(d, 3, 1); binary.BigEndian.PutUint32(d[2*pageSize+4:], 5000) },
		"leaf past the end":  func(d []byte) { setFreelist(d, 3, 2); setTrunk(d, 3, 0, 99) },
		"trunk cycle":        func(d []byte) { setFreelist(d, 3, 2); setTrunk(d, 3, 4); setTrunk(d, 4, 3) },
		"header count wrong": func(d []byte) { setFreelist(d, 3, 5); setTrunk(d, 3, 0, 4) },
		"leaf listed twice":  func(d []byte) { setFreelist(d, 3, 3); setTrunk(d, 3, 0, 4, 4) },
		"partial last page":  nil,
		"header page size 0": func(d []byte) { binary.BigEndian.PutUint16(d[16:], 0) },
	}
	for name, mutate := range cases {
		data := newImage(4)
		if mutate == nil {
			data = data[:len(data)-10]
		} else {
			mutate(data)
		}
		if _, err := readFreelist(data); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// TestNonBtreePageIsNeverUnused checks that a non-freelist page whose type
// byte is not a B-tree page type (such as an overflow page) has no bytes marked
// unused.
func TestNonBtreePageIsNeverUnused(t *testing.T) {
	data := newImage(3)
	binary.BigEndian.PutUint32(data[pageSize:], 3)
	data[pageSize] = 0x00
	unused, kind, err := unusedBytes(data, 2, freelist{trunkLeafCounts: map[int]int{}, leaves: map[int]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(unused, true) || !strings.Contains(kind, "not a b-tree") {
		t.Fatalf("overflow-like page: kind %q", kind)
	}
}

// TestCyclicFreeblocksTerminate checks that a self-referential freeblock chain
// returns an error immediately instead of looping forever.
func TestCyclicFreeblocksTerminate(t *testing.T) {
	data := newImage(2)
	page := data[pageSize:]
	page[0] = 0x0d
	binary.BigEndian.PutUint16(page[1:], 3000)
	binary.BigEndian.PutUint16(page[5:], 2000)
	binary.BigEndian.PutUint16(page[3000:], 3000)
	binary.BigEndian.PutUint16(page[3002:], 8)
	done := make(chan error, 1)
	go func() {
		_, _, err := unusedBytes(data, 2, freelist{trunkLeafCounts: map[int]int{}, leaves: map[int]bool{}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a freeblock pointing at itself: want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the freeblock walk did not terminate")
	}
}

// TestFreeblocksAndGapAreUnused checks that unusedBytes marks the unallocated
// gap between the cell-pointer array and content area and the bodies (after the
// 4-byte header) of freeblocks, leaving live headers and cells unmarked.
func TestFreeblocksAndGapAreUnused(t *testing.T) {
	data := newImage(2)
	page := data[pageSize:]
	page[0] = 0x0d
	binary.BigEndian.PutUint16(page[1:], 3000)
	binary.BigEndian.PutUint16(page[3:], 1)
	binary.BigEndian.PutUint16(page[5:], 2000)
	binary.BigEndian.PutUint16(page[3000:], 0)
	binary.BigEndian.PutUint16(page[3002:], 16)
	unused, kind, err := unusedBytes(data, 2, freelist{trunkLeafCounts: map[int]int{}, leaves: map[int]bool{}})
	if err != nil || !strings.Contains(kind, "b-tree") {
		t.Fatalf("kind %q err %v", kind, err)
	}
	if unused[9] || !unused[10] || !unused[1999] || unused[2000] {
		t.Fatal("the gap between the cell pointers and the content area is wrong")
	}
	if unused[3003] || !unused[3004] || !unused[3015] || unused[3016] {
		t.Fatal("the freeblock body is wrong")
	}
}

// TestPartialPageDifferenceDoesNotPanic checks that diffImages compares partial
// trailing pages and that sizes that differ are reported as different pages.
func TestPartialPageDifferenceDoesNotPanic(t *testing.T) {
	a := make([]byte, pageSize+10)
	b := slices.Clone(a)
	b[pageSize+5] = 1
	d := diffImages(a, b)
	if d.Equal || !slices.Equal(d.DifferingPages, []int{2}) {
		t.Fatalf("diff %+v", d)
	}
	short := diffImages(make([]byte, pageSize), make([]byte, pageSize+3))
	if short.Equal || !slices.Equal(short.DifferingPages, []int{2}) {
		t.Fatalf("diff %+v", short)
	}
}
