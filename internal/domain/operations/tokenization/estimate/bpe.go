package estimate

import (
	"math/bits"
	"sync"
	"unicode/utf8"
)

// Byte-pair encoding turns one piece into tokens by starting from single bytes
// and repeatedly merging the adjacent pair whose concatenation has the lowest
// rank, until no concatenation is a token. Ties go to the leftmost pair, so
// "aaa" merges to "aa", "a" when only "aa" is a token. A piece that is already
// a token, which is most pieces of ordinary text, is a single lookup.

const (
	// smallPiece is the longest piece merged by scanning for the best pair
	// after each merge. That costs a pass over the piece per merge, which
	// beats a heap for the short pieces words make, and its working set lives
	// on the stack. Longer pieces use the heap, so a long unbroken run stays
	// O(n log n) instead of quadratic. BenchmarkCount over 16, 32, 64 and 128
	// found no difference on prose or code, and 32 the fastest on CJK text, whose
	// pieces run to tens of bytes.
	smallPiece = 32
	// maxPiece bounds the working set of one merge. A longer piece, which no
	// text a person wrote contains, is merged in chunks of at most this size,
	// and no token spans two chunks, so its count is only approximate.
	maxPiece = 1 << 18
	// maxPooledPiece is the largest scratch space worth keeping in the pool.
	// One pathological piece must not pin megabytes for the rest of the
	// process, and a piece this long takes far longer to merge than to
	// allocate for.
	maxPooledPiece = 1 << 14
)

// scratch is the working set of mergeLarge: a linked list of parts, indexed
// by the offset where each starts, and a heap of the pairs that can merge.
type scratch struct {
	next, prev []int32
	// rank[i] is the rank of part i merged with the part after it, or noRank.
	rank []uint32
	// heap holds rank<<32 | start, so the smallest entry is the lowest rank
	// and, among equal ranks, the leftmost pair.
	heap []uint64
}

var scratchPool = sync.Pool{New: func() any { return new(scratch) }}

// getScratch returns a scratch for a piece of n bytes. A scratch that has to
// grow grows to the next power of two, so a run of slowly lengthening pieces
// does not reallocate each time.
func getScratch(n int) *scratch {
	sc := scratchPool.Get().(*scratch)
	if cap(sc.next) < n {
		size := max(256, 1<<bits.Len(uint(n-1)))
		sc.next, sc.prev, sc.rank = make([]int32, size), make([]int32, size), make([]uint32, size)
		sc.heap = make([]uint64, 0, size)
	}
	sc.next, sc.prev, sc.rank = sc.next[:n], sc.prev[:n], sc.rank[:n]
	sc.heap = sc.heap[:0]
	return sc
}

func (sc *scratch) release() {
	if cap(sc.next) <= maxPooledPiece {
		scratchPool.Put(sc)
	}
}

// countPiece returns the number of tokens in a non-empty piece.
func (v *vocab) countPiece(p string) int {
	if v.rank(p) != noRank {
		return 1
	}
	if len(p) <= smallPiece {
		var bounds [smallPiece + 1]uint8
		return v.mergeSmall(p, &bounds)
	}
	count := 0
	for len(p) > maxPiece {
		n := chunkLen(p)
		count += v.countLarge(p[:n])
		p = p[n:]
	}
	return count + v.countLarge(p)
}

// chunkLen is how much of a piece longer than maxPiece the next merge takes:
// maxPiece bytes, or a few fewer when they would end inside a rune. A rune cut
// in two leaves each half to be merged as stray bytes, which usually counts
// more tokens than the rune would have, so the cut moves back to the rune's
// first byte. Bytes that are not UTF-8 have no rune to keep whole and are cut
// at maxPiece.
func chunkLen(p string) int {
	for n := maxPiece; n > maxPiece-utf8.UTFMax; n-- {
		if utf8.RuneStart(p[n]) {
			return n
		}
	}
	return maxPiece
}

func (v *vocab) countLarge(p string) int {
	sc := getScratch(len(p))
	defer sc.release()
	return v.mergeLarge(p, sc)
}

// appendPiece appends the tokens of a non-empty piece to dst.
func (v *vocab) appendPiece(dst []uint32, p string) []uint32 {
	if r := v.rank(p); r != noRank {
		return append(dst, r)
	}
	if len(p) <= smallPiece {
		var bounds [smallPiece + 1]uint8
		n := v.mergeSmall(p, &bounds)
		for i := range n {
			dst = append(dst, v.rank(p[bounds[i]:bounds[i+1]]))
		}
		return dst
	}
	for len(p) > maxPiece {
		n := chunkLen(p)
		dst = v.appendLarge(dst, p[:n])
		p = p[n:]
	}
	return v.appendLarge(dst, p)
}

func (v *vocab) appendLarge(dst []uint32, p string) []uint32 {
	sc := getScratch(len(p))
	defer sc.release()
	v.mergeLarge(p, sc)
	for i, n := 0, len(p); i < n; i = int(sc.next[i]) {
		dst = append(dst, v.rank(p[i:sc.next[i]]))
	}
	return dst
}

// mergeSmall merges a piece of up to smallPiece bytes and returns the number
// of parts. The part boundaries are left in bounds[0:parts+1].
func (v *vocab) mergeSmall(p string, bounds *[smallPiece + 1]uint8) int {
	var ranks [smallPiece]uint32
	parts := len(p)
	for i := 0; i <= parts; i++ {
		bounds[i] = uint8(i)
	}
	// ranks[i] is the rank of part i merged with part i+1.
	for i := 0; i+1 < parts; i++ {
		ranks[i] = v.rank(p[i : i+2])
	}
	for parts > 1 {
		best, at := uint32(noRank), -1
		for i, r := range ranks[:parts-1] {
			if r < best {
				best, at = r, i
			}
		}
		if at < 0 {
			break
		}
		// Merge part at with the next by dropping the boundary between them,
		// then rank the two pairs the new part is in.
		copy(bounds[at+1:parts], bounds[at+2:parts+1])
		if at+2 < parts {
			copy(ranks[at+1:], ranks[at+2:parts-1])
		}
		parts--
		if at > 0 {
			ranks[at-1] = v.rank(p[bounds[at-1]:bounds[at+1]])
		}
		if at < parts-1 {
			ranks[at] = v.rank(p[bounds[at]:bounds[at+2]])
		}
	}
	return parts
}

// mergeLarge merges a longer piece with a heap of mergeable pairs, and returns
// the number of parts. A part is named by the offset where it starts and keeps
// that name when it absorbs its successor, so ordering the heap by offset
// breaks rank ties leftmost, as mergeSmall does. Entries are not removed when
// a merge changes them; an entry is stale when the pair it names no longer has
// its rank, which is checked when it is popped. Afterwards sc.next links the
// parts, from offset 0, with len(p) ending the chain.
func (v *vocab) mergeLarge(p string, sc *scratch) int {
	n := len(p)
	next, prev, rank := sc.next, sc.prev, sc.rank
	for i := range n {
		next[i], prev[i] = int32(i+1), int32(i-1)
	}
	rank[n-1] = noRank
	for i := range n - 1 {
		r := v.rank(p[i : i+2])
		rank[i] = r
		if r != noRank {
			sc.heap = append(sc.heap, uint64(r)<<32|uint64(i))
		}
	}
	heapify(sc.heap)
	parts := n
	for len(sc.heap) > 0 {
		e := sc.heap[0]
		sc.heap = popHeap(sc.heap)
		i, r := int32(e), uint32(e>>32)
		if rank[i] != r {
			continue
		}
		// Absorb the successor. A ranked pair always has one.
		j := next[i]
		k := next[j]
		next[i], rank[j] = k, noRank
		if int(k) < n {
			prev[k] = i
		}
		parts--
		rank[i] = noRank
		if int(k) < n {
			if merged := v.rank(p[i:next[k]]); merged != noRank {
				rank[i] = merged
				sc.heap = pushHeap(sc.heap, uint64(merged)<<32|uint64(i))
			}
		}
		if q := prev[i]; q >= 0 {
			rank[q] = noRank
			if merged := v.rank(p[q:k]); merged != noRank {
				rank[q] = merged
				sc.heap = pushHeap(sc.heap, uint64(merged)<<32|uint64(q))
			}
		}
	}
	return parts
}

// The heap is a plain binary min-heap, written out because container/heap
// boxes every entry.

func heapify(h []uint64) {
	for i := len(h)/2 - 1; i >= 0; i-- {
		siftDown(h, i)
	}
}

func pushHeap(h []uint64, e uint64) []uint64 {
	h = append(h, e)
	for i := len(h) - 1; i > 0; {
		parent := (i - 1) / 2
		if h[parent] <= h[i] {
			break
		}
		h[parent], h[i] = h[i], h[parent]
		i = parent
	}
	return h
}

// popHeap removes the smallest entry, which the caller has already read, and
// returns the shortened heap.
func popHeap(h []uint64) []uint64 {
	last := len(h) - 1
	h[0] = h[last]
	h = h[:last]
	siftDown(h, 0)
	return h
}

func siftDown(h []uint64, i int) {
	for {
		child := 2*i + 1
		if child >= len(h) {
			return
		}
		if child+1 < len(h) && h[child+1] < h[child] {
			child++
		}
		if h[i] <= h[child] {
			return
		}
		h[i], h[child] = h[child], h[i]
		i = child
	}
}
