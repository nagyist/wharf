package pwr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/go-errors/errors"
	"github.com/itchio/wharf/counter"
	"github.com/itchio/wharf/pools"
	"github.com/itchio/wharf/pools/fspool"
	"github.com/itchio/wharf/state"
	"github.com/itchio/wharf/tlc"
	"github.com/itchio/wharf/wire"
	"github.com/itchio/wharf/wsync"
)

var (
	// ErrMalformedPatch is returned when a patch could not be parsed
	ErrMalformedPatch = errors.New("malformed patch")

	// ErrIncompatiblePatch is returned when a patch but parsing
	// and applying it is unsupported (e.g. it's a newer version of the format)
	ErrIncompatiblePatch = errors.New("unsupported patch")
)

// VetApplyFunc gives a chance to the caller to abort the application
// before any ops are read/applied - it's the right place to check for
// limits on container size, or number of files, for example.
// By the time it's called, TargetContainer and SourceContainer are
// valid. A VetApplyFunc should only read data from actx, not write to it.
type VetApplyFunc func(actx *ApplyContext) error

// ApplyStats keeps track of various metrics while applying a patch, such as
// operations applied on files, directories, etc.
type ApplyStats struct {
	// files that were touched as a result of applying the patch
	TouchedFiles int
	// files that were not touched at all as a result of applying the patch in-place
	NoopFiles int
	// files that were moved as a result of applying the patch in-place
	MovedFiles int
	// files that were deleted as a result of applying the patch in-place
	DeletedFiles int
	// symlinks that were deleted as a result of applying the patch in-place
	DeletedSymlinks int
	// directories that were deleted as a result of applying the patch in-place
	DeletedDirs int
	// directories that could not be deleted as a result of applying the patch
	LeftDirs  int
	StageSize int64
}

// ApplyContext holds the state while applying a patch
type ApplyContext struct {
	Consumer *state.Consumer

	TargetPath string
	OutputPath string
	InPlace    bool

	TargetContainer *tlc.Container
	TargetPool      wsync.Pool
	SourceContainer *tlc.Container
	OutputPool      wsync.WritablePool

	WoundsPath     string
	WoundsConsumer WoundsConsumer

	VetApply VetApplyFunc

	Signature *SignatureInfo

	Stats ApplyStats

	// internal
	actualOutputPath string
	transpositions   map[string][]*Transposition

	// debug
	debugBrokenRename bool
}

type signature []wsync.BlockHash
type signatureSet map[string]signature
type signatureResult struct {
	path string
	sig  signature
	err  error
}

// GhostKind determines what went missing: a file, a directory, or a symlink
type GhostKind int

const (
	// GhostKindDir indicates that a directory has disappeared between two containers
	GhostKindDir GhostKind = iota + 0xfaf0
	// GhostKindFile indicates that a file has disappeared between two containers
	GhostKindFile
	// GhostKindSymlink indicates that a symbolic link has disappeared between two containers
	GhostKindSymlink
)

// A Ghost is a file, directory, or symlink, that has disappeared from one
// container (target) to the next (source)
type Ghost struct {
	Kind GhostKind
	Path string
}

// ApplyPatch reads a patch, parses it, and generates the new file tree
func (actx *ApplyContext) ApplyPatch(patchReader io.Reader) error {
	actx.actualOutputPath = actx.OutputPath
	if actx.OutputPool == nil {
		if actx.InPlace {
			// applying in-place is a bit tricky: we can't overwrite files in the
			// target directory (old) while we're reading the patch otherwise
			// we might be copying new bytes instead of old bytes into later files
			// so, we rebuild 'touched' files in a staging area
			stagePath := actx.actualOutputPath + "-stage"
			err := os.MkdirAll(stagePath, os.FileMode(0755))
			if err != nil {
				return errors.Wrap(err, 1)
			}

			defer os.RemoveAll(stagePath)
			actx.OutputPath = stagePath
		} else {
			os.MkdirAll(actx.OutputPath, os.FileMode(0755))
		}
	} else {
		if actx.actualOutputPath != "" {
			return fmt.Errorf("cannot specify both OutputPath and OutputPool")
		}
	}

	rawPatchWire := wire.NewReadContext(patchReader)
	err := rawPatchWire.ExpectMagic(PatchMagic)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	header := &PatchHeader{}
	err = rawPatchWire.ReadMessage(header)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	patchWire, err := DecompressWire(rawPatchWire, header.Compression)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	targetContainer := &tlc.Container{}
	err = patchWire.ReadMessage(targetContainer)
	if err != nil {
		return errors.Wrap(err, 1)
	}
	actx.TargetContainer = targetContainer

	sourceContainer := &tlc.Container{}
	err = patchWire.ReadMessage(sourceContainer)
	if err != nil {
		return errors.Wrap(err, 1)
	}
	actx.SourceContainer = sourceContainer

	if actx.VetApply != nil {
		err = actx.VetApply(actx)
		if err != nil {
			return errors.Wrap(err, 1)
		}
	}

	var ghosts []Ghost

	// when not working with a custom output pool
	if actx.OutputPool == nil {
		if actx.InPlace {
			// when working in-place, we have to keep track of which files were deleted
			// from one version to the other, so that we too may delete them in the end.
			ghosts = detectGhosts(actx.SourceContainer, actx.TargetContainer)
		} else {
			// when rebuilding in a fresh directory, there's no need to worry about
			// deleted files, because they won't even exist in the first place.
			err = sourceContainer.Prepare(actx.OutputPath)
			if err != nil {
				return errors.Wrap(err, 1)
			}
		}
	}

	err = actx.patchAll(patchWire, actx.Signature)
	if err != nil {
		return errors.Wrap(err, 1)
	}

	if actx.InPlace {
		err = actx.ensureDirsAndSymlinks(actx.actualOutputPath)
		if err != nil {
			return errors.Wrap(err, 1)
		}

		actx.Stats.StageSize, err = actx.mergeFolders(actx.actualOutputPath, actx.OutputPath)
		if err != nil {
			return errors.Wrap(err, 1)
		}

		err = actx.deleteGhosts(actx.actualOutputPath, ghosts)
		if err != nil {
			return errors.Wrap(err, 1)
		}
		actx.OutputPath = actx.actualOutputPath
	}

	return nil
}

func (actx *ApplyContext) patchAll(patchWire *wire.ReadContext, signature *SignatureInfo) (retErr error) {
	sourceContainer := actx.SourceContainer

	var validatingPool *ValidatingPool
	consumerErrs := make(chan error, 1)

	outputPool := actx.OutputPool
	if outputPool == nil {
		outputPool = fspool.New(sourceContainer, actx.OutputPath)
	}

	if signature != nil {
		validatingPool = &ValidatingPool{
			Pool:      outputPool,
			Container: sourceContainer,
			Signature: signature,
		}

		if actx.WoundsPath != "" {
			validatingPool.Wounds = make(chan *Wound)

			actx.WoundsConsumer = &WoundsWriter{
				WoundsPath: actx.WoundsPath,
			}
		}

		if actx.WoundsConsumer != nil {
			go func() {
				consumerErrs <- actx.WoundsConsumer.Do(signature.Container, validatingPool.Wounds)
			}()
		}

		outputPool = validatingPool
	}

	targetContainer := actx.TargetContainer
	targetPool := actx.TargetPool
	if targetPool == nil {
		if actx.TargetPath == "" {
			return fmt.Errorf("apply: need either TargetPool or TargetPath")
		}
		var cErr error
		targetPool, cErr = pools.New(targetContainer, actx.TargetPath)
		if cErr != nil {
			return cErr
		}
	}

	fileOffset := int64(0)
	sourceBytes := sourceContainer.Size
	onSourceWrite := func(count int64) {
		// we measure patching progress as the number of total bytes written
		// to the source container. no-ops (untouched files) count too, so the
		// progress bar may jump ahead a bit at times, but that's a good surprise
		// measuring progress by bytes of the patch read would just be a different
		// kind of inaccuracy (due to decompression buffers, etc.)
		actx.Consumer.Progress(float64(fileOffset+count) / float64(sourceBytes))
	}

	sctx := mksync()
	sh := &SyncHeader{}

	// transpositions, indexed by TargetPath
	transpositions := make(map[string][]*Transposition)
	actx.transpositions = transpositions

	defer func() {
		var closeErr error
		closeErr = targetPool.Close()
		if closeErr != nil {
			if retErr == nil {
				retErr = errors.Wrap(closeErr, 1)
			}
		}

		closeErr = outputPool.Close()
		if closeErr != nil {
			if retErr == nil {
				retErr = errors.Wrap(closeErr, 1)
			}
		}

		if validatingPool != nil {
			if validatingPool.Wounds != nil {
				close(validatingPool.Wounds)
			}
		}

		if actx.WoundsConsumer != nil {
			taskErr := <-consumerErrs
			if taskErr != nil {
				if retErr == nil {
					retErr = errors.Wrap(taskErr, 1)
				}
			}
		}
	}()

	for fileIndex, f := range sourceContainer.Files {
		actx.Consumer.ProgressLabel(f.Path)
		actx.Consumer.Debug(f.Path)
		fileOffset = f.Offset

		// each series of patch operations is preceded by a SyncHeader giving
		// us the file index - it's a super basic measure to make sure the
		// patch file we're reading and the patching algorithm somewhat agree
		// on what's happening.
		sh.Reset()
		err := patchWire.ReadMessage(sh)
		if err != nil {
			retErr = errors.Wrap(err, 1)
			return
		}

		if sh.FileIndex != int64(fileIndex) {
			fmt.Printf("expected fileIndex = %d, got fileIndex %d\n", fileIndex, sh.FileIndex)
			retErr = errors.Wrap(ErrMalformedPatch, 1)
			return
		}

		ops := make(chan wsync.Operation)
		errc := make(chan error, 1)

		go readOps(patchWire, ops, errc)

		bytesWritten, transposition, err := actx.lazilyPatchFile(sctx, targetContainer, targetPool, sourceContainer, outputPool, sh.FileIndex, onSourceWrite, ops, actx.InPlace)
		if err != nil {
			retErr = errors.Wrap(err, 1)
			return
		}

		if transposition != nil {
			transpositions[transposition.TargetPath] = append(transpositions[transposition.TargetPath], transposition)
		} else {
			actx.Stats.TouchedFiles++
			if bytesWritten != f.Size {
				retErr = fmt.Errorf("%s: expected to write %d bytes, wrote %d bytes", f.Path, f.Size, bytesWritten)
				return
			}
		}

		// using errc to signal the end of processing, rather than having a separate
		// done channel. not sure if there's any upside to either
		err = <-errc
		if err != nil {
			retErr = err
			return
		}
	}

	err := actx.applyTranspositions(transpositions)
	if err != nil {
		retErr = err
		return
	}

	return
}

func (actx *ApplyContext) applyTranspositions(transpositions map[string][]*Transposition) error {
	if len(transpositions) == 0 {
		return nil
	}

	if !actx.InPlace {
		return fmt.Errorf("internal error: found transpositions but not applying in-place")
	}

	for _, group := range transpositions {
		if len(group) == 1 {
			transpo := group[0]
			if transpo.TargetPath == transpo.OutputPath {
				// file wasn't touched at all
				actx.Stats.NoopFiles++
			} else {
				// file was renamed
				oldAbsolutePath := filepath.Join(actx.actualOutputPath, filepath.FromSlash(transpo.TargetPath))
				newAbsolutePath := filepath.Join(actx.actualOutputPath, filepath.FromSlash(transpo.OutputPath))
				err := actx.move(oldAbsolutePath, newAbsolutePath)
				if err != nil {
					return err
				}
				actx.Stats.MovedFiles++
			}
		} else {
			// a file got duplicated!
			var noop *Transposition
			for _, transpo := range group {
				if transpo.TargetPath == transpo.OutputPath {
					noop = transpo
					break
				}
			}

			for i, transpo := range group {
				if noop == nil {
					if i == 0 {
						// arbitrary pick first transposition as being the rename - do
						// all the others as copies first
						continue
					}
				} else if transpo == noop {
					// no need to copy for the noop
					continue
				}

				oldAbsolutePath := filepath.Join(actx.actualOutputPath, filepath.FromSlash(transpo.TargetPath))
				newAbsolutePath := filepath.Join(actx.actualOutputPath, filepath.FromSlash(transpo.OutputPath))
				err := actx.copy(oldAbsolutePath, newAbsolutePath, mkdirBehaviorIfNeeded)
				if err != nil {
					return err
				}
				actx.Stats.TouchedFiles++
			}

			if noop == nil {
				// we treated the first transpo as being the rename, gotta do it now
				transpo := group[0]
				oldAbsolutePath := filepath.Join(actx.actualOutputPath, filepath.FromSlash(transpo.TargetPath))
				newAbsolutePath := filepath.Join(actx.actualOutputPath, filepath.FromSlash(transpo.OutputPath))
				err := actx.move(oldAbsolutePath, newAbsolutePath)
				if err != nil {
					return err
				}
				actx.Stats.MovedFiles++
			} else {
				actx.Stats.NoopFiles++
			}
		}
	}

	return nil
}

func (actx *ApplyContext) move(oldAbsolutePath string, newAbsolutePath string) error {
	err := os.Remove(newAbsolutePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrap(err, 1)
		}
	}

	err = os.MkdirAll(filepath.Dir(newAbsolutePath), os.FileMode(0755))
	if err != nil {
		return errors.Wrap(err, 1)
	}

	if actx.debugBrokenRename {
		err = &os.PathError{}
	} else {
		err = os.Rename(oldAbsolutePath, newAbsolutePath)
	}
	if err != nil {
		cErr := actx.copy(oldAbsolutePath, newAbsolutePath, mkdirBehaviorNever)
		if cErr != nil {
			return cErr
		}

		cErr = os.Remove(oldAbsolutePath)
		if cErr != nil {
			return cErr
		}
	}

	return nil
}

type mkdirBehavior int

const (
	mkdirBehaviorNever mkdirBehavior = 0xf8792 + iota
	mkdirBehaviorIfNeeded
)

func (actx *ApplyContext) copy(oldAbsolutePath string, newAbsolutePath string, mkdirBehavior mkdirBehavior) error {
	if mkdirBehavior == mkdirBehaviorIfNeeded {
		err := os.MkdirAll(filepath.Dir(newAbsolutePath), os.FileMode(0755))
		if err != nil {
			return errors.Wrap(err, 1)
		}
	}

	// fall back to copy + remove
	reader, err := os.Open(oldAbsolutePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	stats, err := reader.Stat()
	if err != nil {
		return err
	}

	writer, err := os.OpenFile(newAbsolutePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, stats.Mode()|tlc.ModeMask)
	if err != nil {
		return err
	}
	defer writer.Close()

	_, err = io.Copy(writer, reader)
	if err != nil {
		return err
	}

	return nil
}

func detectGhosts(sourceContainer *tlc.Container, targetContainer *tlc.Container) []Ghost {
	// first make a map of all the file paths in source, for later lookup
	sourceFileMap := make(map[string]bool)
	for _, f := range sourceContainer.Files {
		sourceFileMap[f.Path] = true
	}
	for _, s := range sourceContainer.Symlinks {
		sourceFileMap[s.Path] = true
	}
	for _, d := range sourceContainer.Dirs {
		sourceFileMap[d.Path] = true
	}

	// then walk through target container paths, if they're not in source, they were deleted
	var ghosts []Ghost
	for _, f := range targetContainer.Files {
		if !sourceFileMap[f.Path] {
			ghosts = append(ghosts, Ghost{
				Kind: GhostKindFile,
				Path: f.Path,
			})
		}
	}
	for _, s := range targetContainer.Symlinks {
		if !sourceFileMap[s.Path] {
			ghosts = append(ghosts, Ghost{
				Kind: GhostKindSymlink,
				Path: s.Path,
			})
		}
	}
	for _, d := range targetContainer.Dirs {
		if !sourceFileMap[d.Path] {
			ghosts = append(ghosts, Ghost{
				Kind: GhostKindDir,
				Path: d.Path,
			})
		}
	}
	return ghosts
}

func (actx *ApplyContext) mergeFolders(outPath string, stagePath string) (int64, error) {
	var filter tlc.FilterFunc = func(fi os.FileInfo) bool {
		return true
	}

	stageContainer, err := tlc.WalkDir(stagePath, filter)
	if err != nil {
		return 0, errors.Wrap(err, 1)
	}

	for _, f := range stageContainer.Files {
		p := filepath.FromSlash(f.Path)
		op := filepath.Join(outPath, p)
		sp := filepath.Join(stagePath, p)

		err := actx.move(sp, op)
		if err != nil {
			return 0, errors.Wrap(err, 1)
		}
	}

	return stageContainer.Size, nil
}

type byDecreasingLength []Ghost

func (s byDecreasingLength) Len() int {
	return len(s)
}

func (s byDecreasingLength) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s byDecreasingLength) Less(i, j int) bool {
	return len(s[j].Path) < len(s[i].Path)
}

func (actx *ApplyContext) deleteGhosts(outPath string, ghosts []Ghost) error {
	sort.Sort(byDecreasingLength(ghosts))

	for _, ghost := range ghosts {
		if len(actx.transpositions[ghost.Path]) > 0 {
			// been renamed
			continue
		}

		op := filepath.Join(outPath, filepath.FromSlash(ghost.Path))

		err := os.Remove(op)
		if err == nil || os.IsNotExist(err) {
			// removed or already removed, good
			switch ghost.Kind {
			case GhostKindDir:
				actx.Stats.DeletedDirs++
			case GhostKindFile:
				actx.Stats.DeletedFiles++
			case GhostKindSymlink:
				actx.Stats.DeletedSymlinks++
			}
		} else {
			if ghost.Kind == GhostKindDir {
				// sometimes we can't delete directories, it's okay
				actx.Stats.LeftDirs++
			} else {
				return errors.Wrap(err, 1)
			}
		}
	}

	return nil
}

// A Transposition is when a file's contents are found wholesale in another
// file - it could be that the file wasn't changed at all, or that it has
// been moved to another folder, or even that a file has been duplicated
// in other locations
type Transposition struct {
	TargetPath string
	OutputPath string
}

func (actx *ApplyContext) lazilyPatchFile(sctx *wsync.Context, targetContainer *tlc.Container, targetPool wsync.Pool, outputContainer *tlc.Container, outputPool wsync.WritablePool,
	fileIndex int64, onSourceWrite counter.CountCallback, ops chan wsync.Operation, inplace bool) (written int64, transposition *Transposition, err error) {

	var writer io.WriteCloser

	defer func() {
		if writer != nil {
			cErr := writer.Close()
			if cErr != nil && err == nil {
				err = cErr
			}
		}
	}()

	var realops chan wsync.Operation

	errs := make(chan error)
	first := true

	for op := range ops {
		if first {
			first = false

			// if the first operation is a blockrange that copies an
			// entire file from target into a file from source that has
			// the same name and size, then it's a no-op!
			if inplace && op.Type == wsync.OpBlockRange && op.BlockIndex == 0 {
				outputFile := outputContainer.Files[fileIndex]
				targetFile := targetContainer.Files[op.FileIndex]
				numOutputBlocks := ComputeNumBlocks(outputFile.Size)

				if op.BlockSpan == numOutputBlocks &&
					outputFile.Size == targetFile.Size {
					transposition = &Transposition{
						TargetPath: targetFile.Path,
						OutputPath: outputFile.Path,
					}
				}
			}

			if transposition != nil {
				go func() {
					errs <- nil
				}()
			} else {
				realops = make(chan wsync.Operation)

				writer, err = outputPool.GetWriter(fileIndex)
				if err != nil {
					errs <- errors.Wrap(err, 1)
				} else {
					writeCounter := counter.NewWriterCallback(onSourceWrite, writer)

					go func() {
						applyErr := sctx.ApplyPatch(writeCounter, targetPool, realops)
						if applyErr != nil {
							errs <- applyErr
							return
						}

						written = writeCounter.Count()
						errs <- nil
					}()
				}
			}
		}

		// if not a transposition, relay errors
		if transposition == nil {
			select {
			case cErr := <-errs:
				// if we get an error here, ApplyPatch failed so we no longer need to close realops
				if cErr != nil {
					return 0, nil, errors.Wrap(cErr, 1)
				}
			case realops <- op:
				// muffin
			}
		}
	}

	if transposition == nil {
		close(realops)
	}

	err = <-errs
	if err != nil {
		return 0, nil, errors.Wrap(err, 1)
	}

	return
}

func readOps(rc *wire.ReadContext, ops chan wsync.Operation, errc chan error) {
	defer close(ops)
	rop := &SyncOp{}

	readingOps := true
	for readingOps {
		rop.Reset()
		err := rc.ReadMessage(rop)
		if err != nil {
			errc <- errors.Wrap(err, 1)
			return
		}

		switch rop.Type {
		case SyncOp_BLOCK_RANGE:
			ops <- wsync.Operation{
				Type:       wsync.OpBlockRange,
				FileIndex:  rop.FileIndex,
				BlockIndex: rop.BlockIndex,
				BlockSpan:  rop.BlockSpan,
			}

		case SyncOp_DATA:
			ops <- wsync.Operation{
				Type: wsync.OpData,
				Data: rop.Data,
			}

		default:
			switch rop.Type {
			case SyncOp_HEY_YOU_DID_IT:
				// series of patching operations always end with a SyncOp_HEY_YOU_DID_IT.
				// this helps detect truncated patch files, and, again, basic boundary
				// safety measures are cheap and reassuring.
				readingOps = false
			default:
				errc <- errors.Wrap(ErrMalformedPatch, 1)
				return
			}
		}
	}

	errc <- nil
}

func (actx *ApplyContext) ensureDirsAndSymlinks(actualOutputPath string) error {
	for _, dir := range actx.SourceContainer.Dirs {
		path := filepath.Join(actualOutputPath, filepath.FromSlash(dir.Path))

		err := os.MkdirAll(path, 0755)
		if err != nil {
			// If path is already a directory, MkdirAll does nothing and returns nil.
			// so if we get a non-nil error, we know it's serious business (permissions, etc.)
			return err
		}
	}

	for _, symlink := range actx.SourceContainer.Symlinks {
		path := filepath.Join(actualOutputPath, filepath.FromSlash(symlink.Path))
		dest, err := os.Readlink(path)
		if err != nil {
			if os.IsNotExist(err) {
				// symlink was missing
				err = os.Symlink(filepath.FromSlash(symlink.Dest), path)
				if err != nil {
					return err
				}
			} else {
				return err
			}
		}

		// symlink is there
		if dest != filepath.FromSlash(symlink.Dest) {
			// wrong dest, fixing that
			err = os.Remove(path)
			if err != nil {
				return err
			}

			err = os.Symlink(filepath.FromSlash(symlink.Dest), path)
			if err != nil {
				return err
			}

			return nil
		}
	}

	return nil
}
