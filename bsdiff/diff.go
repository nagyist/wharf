package bsdiff

import (
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"runtime"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/golang/protobuf/proto"
	"github.com/itchio/wharf/state"
)

// MaxFileSize is the largest size bsdiff will diff (for both old and new file)
const MaxFileSize = int64(math.MaxInt32 - 1)

// MaxMessageSize is the maximum amount of bytes that will be stored
// in a protobuf message generated by bsdiff. This enable friendlier streaming apply
// at a small storage cost
const MaxMessageSize int64 = 16 * 1024 * 1024

// DiffContext holds settings for the diff process, along with some
// internal storage: re-using a diff context is good to avoid GC thrashing
// (but never do it concurrently!)
type DiffContext struct {
	db []byte // diff bytes
	eb []byte // extra bytes

	// DebugMem enables printing memory usage statistics at various points in the
	// diffing process.
	DebugMem bool
}

// WriteMessageFunc should write a given protobuf message and relay any errors
// No reference to the given message can be kept, as its content may be modified
// after WriteMessageFunc returns. See the `wire` package for an example implementation.
type WriteMessageFunc func(msg proto.Message) (err error)

// Do computes the difference between old and new, according to the bsdiff
// algorithm, and writes the result to patch.
func (ctx *DiffContext) Do(old, new io.Reader, writeMessage WriteMessageFunc, consumer *state.Consumer) error {
	var memstats *runtime.MemStats

	if ctx.DebugMem {
		memstats = &runtime.MemStats{}
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes at start of bsdiff: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	if ctx.db == nil {
		ctx.db = make([]byte, MaxMessageSize)
	}
	if ctx.eb == nil {
		ctx.eb = make([]byte, MaxMessageSize)
	}

	obuf, err := ioutil.ReadAll(old)
	if err != nil {
		return err
	}
	if int64(len(obuf)) > MaxFileSize {
		return fmt.Errorf("bsdiff: old file too large (%s > %s)", humanize.IBytes(uint64(len(obuf))), humanize.IBytes(uint64(MaxFileSize)))
	}
	obuflen := int32(len(obuf))

	nbuf, err := ioutil.ReadAll(new)
	if err != nil {
		return err
	}
	if int64(len(nbuf)) > MaxFileSize {
		return fmt.Errorf("bsdiff: new file too large (%s > %s)", humanize.IBytes(uint64(len(nbuf))), humanize.IBytes(uint64(MaxFileSize)))
	}
	nbuflen := int32(len(nbuf))

	if ctx.DebugMem {
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes after ReadAll: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	var lenf int32
	startTime := time.Now()

	I := qsufsort(obuf, consumer)

	duration := time.Since(startTime)
	consumer.Debugf("Suffix sorting done in %s", duration)

	if ctx.DebugMem {
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes after qsufsort: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	// FIXME: the streaming format allows us to allocate less than that
	db := make([]byte, len(nbuf))
	eb := make([]byte, len(nbuf))

	bsdc := &Control{}

	consumer.ProgressLabel("Scanning...")

	// Compute the differences, writing ctrl as we go
	var scan, pos, length int32
	var lastscan, lastpos, lastoffset int32
	for scan < nbuflen {
		var oldscore int32
		scan += length

		progress := float64(scan) / float64(nbuflen)
		consumer.Progress(progress)

		for scsc := scan; scan < nbuflen; scan++ {
			pos, length = search(I, obuf, nbuf[scan:], 0, obuflen)

			for ; scsc < scan+length; scsc++ {
				if scsc+lastoffset < obuflen &&
					obuf[scsc+lastoffset] == nbuf[scsc] {
					oldscore++
				}
			}

			if (length == oldscore && length != 0) || length > oldscore+8 {
				break
			}

			if scan+lastoffset < obuflen && obuf[scan+lastoffset] == nbuf[scan] {
				oldscore--
			}
		}

		if length != oldscore || scan == nbuflen {
			var s, Sf int32
			lenf = 0
			for i := int32(0); lastscan+i < scan && lastpos+i < obuflen; {
				if obuf[lastpos+i] == nbuf[lastscan+i] {
					s++
				}
				i++
				if s*2-i > Sf*2-lenf {
					Sf = s
					lenf = i
				}
			}

			lenb := int32(0)
			if scan < nbuflen {
				var s, Sb int32
				for i := int32(1); (scan >= lastscan+i) && (pos >= i); i++ {
					if obuf[pos-i] == nbuf[scan-i] {
						s++
					}
					if s*2-i > Sb*2-lenb {
						Sb = s
						lenb = i
					}
				}
			}

			if lastscan+lenf > scan-lenb {
				overlap := (lastscan + lenf) - (scan - lenb)
				s := int32(0)
				Ss := int32(0)
				lens := int32(0)
				for i := int32(0); i < overlap; i++ {
					if nbuf[lastscan+lenf-overlap+i] == obuf[lastpos+lenf-overlap+i] {
						s++
					}
					if nbuf[scan-lenb+i] == obuf[pos-lenb+i] {
						s--
					}
					if s > Ss {
						Ss = s
						lens = i + 1
					}
				}

				lenf += lens - overlap
				lenb -= lens
			}

			for i := int32(0); i < lenf; i++ {
				db[i] = nbuf[lastscan+i] - obuf[lastpos+i]
			}
			for i := int32(0); i < (scan-lenb)-(lastscan+lenf); i++ {
				eb[i] = nbuf[lastscan+lenf+i]
			}

			bsdc.Add = db[:lenf]
			bsdc.Copy = eb[:(scan-lenb)-(lastscan+lenf)]
			bsdc.Seek = int64((pos - lenb) - (lastpos + lenf))

			err := writeMessage(bsdc)
			if err != nil {
				return err
			}

			lastscan = scan - lenb
			lastpos = pos - lenb
			lastoffset = pos - scan
		}
	}

	if ctx.DebugMem {
		runtime.ReadMemStats(memstats)
		consumer.Debugf("Allocated bytes after scan: %s (%s total)", humanize.IBytes(uint64(memstats.Alloc)), humanize.IBytes(uint64(memstats.TotalAlloc)))
	}

	// Write sentinel control message
	bsdc.Reset()
	bsdc.Eof = true
	err = writeMessage(bsdc)
	if err != nil {
		return err
	}

	return nil
}
