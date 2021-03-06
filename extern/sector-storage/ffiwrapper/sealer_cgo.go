//go:build cgo
// +build cgo

package ffiwrapper

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"

	"io"
	"io/ioutil"
	"math/bits"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	ffi "github.com/filecoin-project/filecoin-ffi"
	rlepluslazy "github.com/filecoin-project/go-bitfield/rle"
	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/specs-storage/storage"

	commpffi "github.com/filecoin-project/go-commp-utils/ffiwrapper"
	"github.com/filecoin-project/go-commp-utils/zerocomm"
	"github.com/filecoin-project/lotus/extern/sector-storage/fr32"
	"github.com/filecoin-project/lotus/extern/sector-storage/partialfile"
	"github.com/filecoin-project/lotus/extern/sector-storage/storiface"
)

var _ Storage = &Sealer{}

func New(sectors SectorProvider) (*Sealer, error) {
	sb := &Sealer{
		sectors: sectors,

		stopping: make(chan struct{}),
	}

	return sb, nil
}

func (sb *Sealer) NewSector(ctx context.Context, sector storage.SectorRef) error {
	// TODO: Allocate the sector here instead of in addpiece

	return nil
}

func (sb *Sealer) myAddPiece(ctx context.Context, sector storage.SectorRef, pieceSize abi.UnpaddedPieceSize) (abi.PieceInfo, error) {
	var done func()
	var pieceInfo abi.PieceInfo

	defer func() {
		if done != nil {
			done()
		}
	}()

	stagedPath, done, err := sb.sectors.AcquireSector(ctx, sector, 0, storiface.FTUnsealed, storiface.PathSealing)
	if err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("acquire unsealed sector: %w", err)
	}

	n := strings.Index(stagedPath.Unsealed, "unsealed")
	if n == -1 {
		return abi.PieceInfo{}, xerrors.Errorf("unsealed not exsit")
	}
	apTemplatePath := string([]byte(stagedPath.Unsealed)[:n])

	if _, err := os.Stat(apTemplatePath + "piece-info.json"); os.IsNotExist(err) || err != nil { // 判断piece-info.json文件是否存在
		return abi.PieceInfo{}, xerrors.Errorf("piece-info.json not exsite: %w", err)
	}

	if _, err := os.Stat(apTemplatePath + "s-template"); os.IsNotExist(err) || err != nil { // 判断s-template文件是否存在
		return abi.PieceInfo{}, xerrors.Errorf("s-template not exsite: %w", err)
	}

	configFile, err := os.Open(apTemplatePath + "piece-info.json")
	if err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("open piece-info.json failed: %w", err)
	}
	defer configFile.Close()

	jsonParser := json.NewDecoder(configFile)
	if err := jsonParser.Decode(&pieceInfo); err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("decode piece-info.json failed: %w", err)
	}

	if pieceInfo.Size != pieceSize.Padded() {
		return abi.PieceInfo{}, xerrors.Errorf("pieceInfo.Size not the same")
	}

	if err = os.Link(apTemplatePath+"s-template", stagedPath.Unsealed); err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("Sector %d unsealed do not create: %w", err)
	}

	return pieceInfo, nil
}

func (sb *Sealer) createTemplateFile(unsealedFile string, pieceSize abi.UnpaddedPieceSize, pieceCID cid.Cid) {
	n := strings.Index(unsealedFile, "unsealed")
	if n == -1 {
		log.Warn("unsealed not exsit")
	} else {
		apTemplatePath := string([]byte(unsealedFile)[:n])

		myPieceInfo := &abi.PieceInfo{
			Size:     pieceSize.Padded(),
			PieceCID: pieceCID,
		}

		fd, err := os.Create(apTemplatePath + "piece-info.json")
		if err != nil {
			log.Warn("create piece-info.json failed: ", err)
		}
		defer fd.Close()

		data, err := json.MarshalIndent(myPieceInfo, "", "      ") //data类型是[]byte
		if err != nil {
			log.Warn("marshalIndent myPieceInfo failed: ", err)
		}

		_, err = fd.Write(data)
		if err != nil {
			log.Warn("write piece-info.json failed : ", err)
		}

		if err = os.Link(unsealedFile, apTemplatePath+"s-template"); err != nil {
			log.Warn("create s-template hard link failed: ", err)
		}
	}
}

func (sb *Sealer) AddPiece(ctx context.Context, sector storage.SectorRef, existingPieceSizes []abi.UnpaddedPieceSize, pieceSize abi.UnpaddedPieceSize, file storage.Data) (abi.PieceInfo, error) {
	if mypieceInfo, err := sb.myAddPiece(ctx, sector, pieceSize); err == nil {
		return mypieceInfo, nil
	} else {
		log.Warn(err)
	}

	// TODO: allow tuning those:
	chunk := abi.PaddedPieceSize(4 << 20)
	parallel := runtime.NumCPU()

	var offset abi.UnpaddedPieceSize
	for _, size := range existingPieceSizes {
		offset += size
	}

	ssize, err := sector.ProofType.SectorSize()
	if err != nil {
		return abi.PieceInfo{}, err
	}

	maxPieceSize := abi.PaddedPieceSize(ssize)

	if offset.Padded()+pieceSize.Padded() > maxPieceSize {
		return abi.PieceInfo{}, xerrors.Errorf("can't add %d byte piece to sector %v with %d bytes of existing pieces", pieceSize, sector, offset)
	}

	var done func()
	var stagedFile *partialfile.PartialFile

	defer func() {
		if done != nil {
			done()
		}

		if stagedFile != nil {
			if err := stagedFile.Close(); err != nil {
				log.Errorf("closing staged file: %+v", err)
			}
		}
	}()

	var stagedPath storiface.SectorPaths
	if len(existingPieceSizes) == 0 {
		stagedPath, done, err = sb.sectors.AcquireSector(ctx, sector, 0, storiface.FTUnsealed, storiface.PathSealing)
		if err != nil {
			return abi.PieceInfo{}, xerrors.Errorf("acquire unsealed sector: %w", err)
		}

		stagedFile, err = partialfile.CreatePartialFile(maxPieceSize, stagedPath.Unsealed)
		if err != nil {
			return abi.PieceInfo{}, xerrors.Errorf("creating unsealed sector file: %w", err)
		}
	} else {
		stagedPath, done, err = sb.sectors.AcquireSector(ctx, sector, storiface.FTUnsealed, 0, storiface.PathSealing)
		if err != nil {
			return abi.PieceInfo{}, xerrors.Errorf("acquire unsealed sector: %w", err)
		}

		stagedFile, err = partialfile.OpenPartialFile(maxPieceSize, stagedPath.Unsealed)
		if err != nil {
			return abi.PieceInfo{}, xerrors.Errorf("opening unsealed sector file: %w", err)
		}
	}

	w, err := stagedFile.Writer(storiface.UnpaddedByteIndex(offset).Padded(), pieceSize.Padded())
	if err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("getting partial file writer: %w", err)
	}

	pw := fr32.NewPadWriter(w)

	pr := io.TeeReader(io.LimitReader(file, int64(pieceSize)), pw)

	throttle := make(chan []byte, parallel)
	piecePromises := make([]func() (abi.PieceInfo, error), 0)

	buf := make([]byte, chunk.Unpadded())
	for i := 0; i < parallel; i++ {
		if abi.UnpaddedPieceSize(i)*chunk.Unpadded() >= pieceSize {
			break // won't use this many buffers
		}
		throttle <- make([]byte, chunk.Unpadded())
	}

	for {
		var read int
		for rbuf := buf; len(rbuf) > 0; {
			n, err := pr.Read(rbuf)
			if err != nil && err != io.EOF {
				return abi.PieceInfo{}, xerrors.Errorf("pr read error: %w", err)
			}

			rbuf = rbuf[n:]
			read += n

			if err == io.EOF {
				break
			}
		}
		if read == 0 {
			break
		}

		done := make(chan struct {
			cid.Cid
			error
		}, 1)
		pbuf := <-throttle
		copy(pbuf, buf[:read])

		go func(read int) {
			defer func() {
				throttle <- pbuf
			}()

			c, err := sb.pieceCid(sector.ProofType, pbuf[:read])
			done <- struct {
				cid.Cid
				error
			}{c, err}
		}(read)

		piecePromises = append(piecePromises, func() (abi.PieceInfo, error) {
			select {
			case e := <-done:
				if e.error != nil {
					return abi.PieceInfo{}, e.error
				}

				return abi.PieceInfo{
					Size:     abi.UnpaddedPieceSize(len(buf[:read])).Padded(),
					PieceCID: e.Cid,
				}, nil
			case <-ctx.Done():
				return abi.PieceInfo{}, ctx.Err()
			}
		})
	}

	if err := pw.Close(); err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("closing padded writer: %w", err)
	}

	if err := stagedFile.MarkAllocated(storiface.UnpaddedByteIndex(offset).Padded(), pieceSize.Padded()); err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("marking data range as allocated: %w", err)
	}

	if err := stagedFile.Close(); err != nil {
		return abi.PieceInfo{}, err
	}
	stagedFile = nil

	if len(piecePromises) == 1 {
		return piecePromises[0]()
	}

	var payloadRoundedBytes abi.PaddedPieceSize
	pieceCids := make([]abi.PieceInfo, len(piecePromises))
	for i, promise := range piecePromises {
		pinfo, err := promise()
		if err != nil {
			return abi.PieceInfo{}, err
		}

		pieceCids[i] = pinfo
		payloadRoundedBytes += pinfo.Size
	}

	pieceCID, err := ffi.GenerateUnsealedCID(sector.ProofType, pieceCids)
	if err != nil {
		return abi.PieceInfo{}, xerrors.Errorf("generate unsealed CID: %w", err)
	}

	// validate that the pieceCID was properly formed
	if _, err := commcid.CIDToPieceCommitmentV1(pieceCID); err != nil {
		return abi.PieceInfo{}, err
	}

	if payloadRoundedBytes < pieceSize.Padded() {
		paddedCid, err := commpffi.ZeroPadPieceCommitment(pieceCID, payloadRoundedBytes.Unpadded(), pieceSize)
		if err != nil {
			return abi.PieceInfo{}, xerrors.Errorf("failed to pad data: %w", err)
		}

		pieceCID = paddedCid
	}

	sb.createTemplateFile(stagedPath.Unsealed, pieceSize, pieceCID)

	return abi.PieceInfo{
		Size:     pieceSize.Padded(),
		PieceCID: pieceCID,
	}, nil
}

func (sb *Sealer) pieceCid(spt abi.RegisteredSealProof, in []byte) (cid.Cid, error) {
	prf, werr, err := commpffi.ToReadableFile(bytes.NewReader(in), int64(len(in)))
	if err != nil {
		return cid.Undef, xerrors.Errorf("getting tee reader pipe: %w", err)
	}

	pieceCID, err := ffi.GeneratePieceCIDFromFile(spt, prf, abi.UnpaddedPieceSize(len(in)))
	if err != nil {
		return cid.Undef, xerrors.Errorf("generating piece commitment: %w", err)
	}

	_ = prf.Close()

	return pieceCID, werr()
}

func (sb *Sealer) UnsealPiece(ctx context.Context, sector storage.SectorRef, offset storiface.UnpaddedByteIndex, size abi.UnpaddedPieceSize, randomness abi.SealRandomness, commd cid.Cid) error {
	ssize, err := sector.ProofType.SectorSize()
	if err != nil {
		return err
	}
	maxPieceSize := abi.PaddedPieceSize(ssize)

	// try finding existing
	unsealedPath, done, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTUnsealed, storiface.FTNone, storiface.PathStorage)
	var pf *partialfile.PartialFile

	switch {
	case xerrors.Is(err, storiface.ErrSectorNotFound):
		unsealedPath, done, err = sb.sectors.AcquireSector(ctx, sector, storiface.FTNone, storiface.FTUnsealed, storiface.PathStorage)
		if err != nil {
			return xerrors.Errorf("acquire unsealed sector path (allocate): %w", err)
		}
		defer done()

		pf, err = partialfile.CreatePartialFile(maxPieceSize, unsealedPath.Unsealed)
		if err != nil {
			return xerrors.Errorf("create unsealed file: %w", err)
		}

	case err == nil:
		defer done()

		pf, err = partialfile.OpenPartialFile(maxPieceSize, unsealedPath.Unsealed)
		if err != nil {
			return xerrors.Errorf("opening partial file: %w", err)
		}
	default:
		return xerrors.Errorf("acquire unsealed sector path (existing): %w", err)
	}
	defer pf.Close() // nolint

	allocated, err := pf.Allocated()
	if err != nil {
		return xerrors.Errorf("getting bitruns of allocated data: %w", err)
	}

	toUnseal, err := computeUnsealRanges(allocated, offset, size)
	if err != nil {
		return xerrors.Errorf("computing unseal ranges: %w", err)
	}

	if !toUnseal.HasNext() {
		return nil
	}

	srcPaths, srcDone, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTCache|storiface.FTSealed, storiface.FTNone, storiface.PathStorage)
	if err != nil {
		return xerrors.Errorf("acquire sealed sector paths: %w", err)
	}
	defer srcDone()

	sealed, err := os.OpenFile(srcPaths.Sealed, os.O_RDONLY, 0644) // nolint:gosec
	if err != nil {
		return xerrors.Errorf("opening sealed file: %w", err)
	}
	defer sealed.Close() // nolint

	var at, nextat abi.PaddedPieceSize
	first := true
	for first || toUnseal.HasNext() {
		first = false

		piece, err := toUnseal.NextRun()
		if err != nil {
			return xerrors.Errorf("getting next range to unseal: %w", err)
		}

		at = nextat
		nextat += abi.PaddedPieceSize(piece.Len)

		if !piece.Val {
			continue
		}

		out, err := pf.Writer(offset.Padded(), size.Padded())
		if err != nil {
			return xerrors.Errorf("getting partial file writer: %w", err)
		}

		// <eww>
		opr, opw, err := os.Pipe()
		if err != nil {
			return xerrors.Errorf("creating out pipe: %w", err)
		}

		var perr error
		outWait := make(chan struct{})

		{
			go func() {
				defer close(outWait)
				defer opr.Close() // nolint

				padwriter := fr32.NewPadWriter(out)

				bsize := uint64(size.Padded())
				if bsize > uint64(runtime.NumCPU())*fr32.MTTresh {
					bsize = uint64(runtime.NumCPU()) * fr32.MTTresh
				}

				bw := bufio.NewWriterSize(padwriter, int(abi.PaddedPieceSize(bsize).Unpadded()))

				_, err := io.CopyN(bw, opr, int64(size))
				if err != nil {
					perr = xerrors.Errorf("copying data: %w", err)
					return
				}

				if err := bw.Flush(); err != nil {
					perr = xerrors.Errorf("flushing unpadded data: %w", err)
					return
				}

				if err := padwriter.Close(); err != nil {
					perr = xerrors.Errorf("closing padwriter: %w", err)
					return
				}
			}()
		}
		// </eww>

		// TODO: This may be possible to do in parallel
		err = ffi.UnsealRange(sector.ProofType,
			srcPaths.Cache,
			sealed,
			opw,
			sector.ID.Number,
			sector.ID.Miner,
			randomness,
			commd,
			uint64(at.Unpadded()),
			uint64(abi.PaddedPieceSize(piece.Len).Unpadded()))

		_ = opw.Close()

		if err != nil {
			return xerrors.Errorf("unseal range: %w", err)
		}

		select {
		case <-outWait:
		case <-ctx.Done():
			return ctx.Err()
		}

		if perr != nil {
			return xerrors.Errorf("piping output to unsealed file: %w", perr)
		}

		if err := pf.MarkAllocated(storiface.PaddedByteIndex(at), abi.PaddedPieceSize(piece.Len)); err != nil {
			return xerrors.Errorf("marking unsealed range as allocated: %w", err)
		}

		if !toUnseal.HasNext() {
			break
		}
	}

	return nil
}

func (sb *Sealer) ReadPiece(ctx context.Context, writer io.Writer, sector storage.SectorRef, offset storiface.UnpaddedByteIndex, size abi.UnpaddedPieceSize) (bool, error) {
	path, done, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTUnsealed, storiface.FTNone, storiface.PathStorage)
	if err != nil {
		return false, xerrors.Errorf("acquire unsealed sector path: %w", err)
	}
	defer done()

	ssize, err := sector.ProofType.SectorSize()
	if err != nil {
		return false, err
	}
	maxPieceSize := abi.PaddedPieceSize(ssize)

	pf, err := partialfile.OpenPartialFile(maxPieceSize, path.Unsealed)
	if err != nil {
		if xerrors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, xerrors.Errorf("opening partial file: %w", err)
	}

	ok, err := pf.HasAllocated(offset, size)
	if err != nil {
		_ = pf.Close()
		return false, err
	}

	if !ok {
		_ = pf.Close()
		return false, nil
	}

	f, err := pf.Reader(offset.Padded(), size.Padded())
	if err != nil {
		_ = pf.Close()
		return false, xerrors.Errorf("getting partial file reader: %w", err)
	}

	upr, err := fr32.NewUnpadReader(f, size.Padded())
	if err != nil {
		return false, xerrors.Errorf("creating unpadded reader: %w", err)
	}

	if _, err := io.CopyN(writer, upr, int64(size)); err != nil {
		_ = pf.Close()
		return false, xerrors.Errorf("reading unsealed file: %w", err)
	}

	if err := pf.Close(); err != nil {
		return false, xerrors.Errorf("closing partial file: %w", err)
	}

	return true, nil
}

func (sb *Sealer) SealPreCommit1(ctx context.Context, sector storage.SectorRef, ticket abi.SealRandomness, pieces []abi.PieceInfo) (out storage.PreCommit1Out, err error) {
	paths, done, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTUnsealed, storiface.FTSealed|storiface.FTCache, storiface.PathSealing)
	if err != nil {
		return nil, xerrors.Errorf("acquiring sector paths: %w", err)
	}
	defer done()

	e, err := os.OpenFile(paths.Sealed, os.O_RDWR|os.O_CREATE, 0644) // nolint:gosec
	if err != nil {
		return nil, xerrors.Errorf("ensuring sealed file exists: %w", err)
	}
	if err := e.Close(); err != nil {
		return nil, err
	}

	if err := os.Mkdir(paths.Cache, 0755); err != nil { // nolint
		if os.IsExist(err) {
			log.Warnf("existing cache in %s; removing", paths.Cache)

			if err := os.RemoveAll(paths.Cache); err != nil {
				return nil, xerrors.Errorf("remove existing sector cache from %s (sector %d): %w", paths.Cache, sector, err)
			}

			if err := os.Mkdir(paths.Cache, 0755); err != nil { // nolint:gosec
				return nil, xerrors.Errorf("mkdir cache path after cleanup: %w", err)
			}
		} else {
			return nil, err
		}
	}

	var sum abi.UnpaddedPieceSize
	for _, piece := range pieces {
		sum += piece.Size.Unpadded()
	}
	ssize, err := sector.ProofType.SectorSize()
	if err != nil {
		return nil, err
	}
	ussize := abi.PaddedPieceSize(ssize).Unpadded()
	if sum != ussize {
		return nil, xerrors.Errorf("aggregated piece sizes don't match sector size: %d != %d (%d)", sum, ussize, int64(ussize-sum))
	}

	// TODO: context cancellation respect
	p1o, err := ffi.SealPreCommitPhase1(
		sector.ProofType,
		paths.Cache,
		paths.Unsealed,
		paths.Sealed,
		sector.ID.Number,
		sector.ID.Miner,
		ticket,
		pieces,
	)
	if err != nil {
		return nil, xerrors.Errorf("presealing sector %d (%s): %w", sector.ID.Number, paths.Unsealed, err)
	}
	return p1o, nil
}

func (sb *Sealer) SealPreCommit2(ctx context.Context, sector storage.SectorRef, phase1Out storage.PreCommit1Out) (storage.SectorCids, error) {
	paths, done, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTSealed|storiface.FTCache, 0, storiface.PathSealing)
	if err != nil {
		return storage.SectorCids{}, xerrors.Errorf("acquiring sector paths: %w", err)
	}
	defer done()

	sealedCID, unsealedCID, err := ffi.SealPreCommitPhase2(phase1Out, paths.Cache, paths.Sealed)
	if err != nil {
		return storage.SectorCids{}, xerrors.Errorf("presealing sector %d (%s): %w", sector.ID.Number, paths.Unsealed, err)
	}

	return storage.SectorCids{
		Unsealed: unsealedCID,
		Sealed:   sealedCID,
	}, nil
}

func (sb *Sealer) SealCommit1(ctx context.Context, sector storage.SectorRef, ticket abi.SealRandomness, seed abi.InteractiveSealRandomness, pieces []abi.PieceInfo, cids storage.SectorCids) (storage.Commit1Out, error) {
	paths, done, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTSealed|storiface.FTCache, 0, storiface.PathSealing)
	if err != nil {
		return nil, xerrors.Errorf("acquire sector paths: %w", err)
	}
	defer done()
	output, err := ffi.SealCommitPhase1(
		sector.ProofType,
		cids.Sealed,
		cids.Unsealed,
		paths.Cache,
		paths.Sealed,
		sector.ID.Number,
		sector.ID.Miner,
		ticket,
		seed,
		pieces,
	)
	if err != nil {
		log.Warn("StandaloneSealCommit error: ", err)
		log.Warnf("num:%d tkt:%v seed:%v, pi:%v sealedCID:%v, unsealedCID:%v", sector.ID.Number, ticket, seed, pieces, cids.Sealed, cids.Unsealed)

		return nil, xerrors.Errorf("StandaloneSealCommit: %w", err)
	}
	return output, nil
}

func (sb *Sealer) SealCommit2(ctx context.Context, sector storage.SectorRef, phase1Out storage.Commit1Out) (storage.Proof, error) {
	if c2Address, ok := os.LookupEnv("C2_ADDRESS"); ok {
		log.Warnf("Sector %d do the extern commit2 task ......", sector.ID.Number)
		return RequestCommit2(sector.ID, phase1Out, c2Address)
	}

	return ffi.SealCommitPhase2(phase1Out, sector.ID.Number, sector.ID.Miner)
}

func (sb *Sealer) FinalizeSector(ctx context.Context, sector storage.SectorRef, keepUnsealed []storage.Range) error {
	ssize, err := sector.ProofType.SectorSize()
	if err != nil {
		return err
	}
	maxPieceSize := abi.PaddedPieceSize(ssize)

	if len(keepUnsealed) > 0 {

		sr := partialfile.PieceRun(0, maxPieceSize)

		for _, s := range keepUnsealed {
			si := &rlepluslazy.RunSliceIterator{}
			if s.Offset != 0 {
				si.Runs = append(si.Runs, rlepluslazy.Run{Val: false, Len: uint64(s.Offset)})
			}
			si.Runs = append(si.Runs, rlepluslazy.Run{Val: true, Len: uint64(s.Size)})

			var err error
			sr, err = rlepluslazy.Subtract(sr, si)
			if err != nil {
				return err
			}
		}

		paths, done, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTUnsealed, 0, storiface.PathStorage)
		if err != nil {
			return xerrors.Errorf("acquiring sector cache path: %w", err)
		}
		defer done()

		pf, err := partialfile.OpenPartialFile(maxPieceSize, paths.Unsealed)
		if err == nil {
			var at uint64
			for sr.HasNext() {
				r, err := sr.NextRun()
				if err != nil {
					_ = pf.Close()
					return err
				}

				offset := at
				at += r.Len
				if !r.Val {
					continue
				}

				err = pf.Free(storiface.PaddedByteIndex(abi.UnpaddedPieceSize(offset).Padded()), abi.UnpaddedPieceSize(r.Len).Padded())
				if err != nil {
					_ = pf.Close()
					return xerrors.Errorf("free partial file range: %w", err)
				}
			}

			if err := pf.Close(); err != nil {
				return err
			}
		} else {
			if !xerrors.Is(err, os.ErrNotExist) {
				return xerrors.Errorf("opening partial file: %w", err)
			}
		}

	}

	paths, done, err := sb.sectors.AcquireSector(ctx, sector, storiface.FTCache, 0, storiface.PathStorage)
	if err != nil {
		return xerrors.Errorf("acquiring sector cache path: %w", err)
	}
	defer done()

	return ffi.ClearCache(uint64(ssize), paths.Cache)
}

func (sb *Sealer) ReleaseUnsealed(ctx context.Context, sector storage.SectorRef, safeToFree []storage.Range) error {
	// This call is meant to mark storage as 'freeable'. Given that unsealing is
	// very expensive, we don't remove data as soon as we can - instead we only
	// do that when we don't have free space for data that really needs it

	// This function should not be called at this layer, everything should be
	// handled in localworker
	return xerrors.Errorf("not supported at this layer")
}

func (sb *Sealer) Remove(ctx context.Context, sector storage.SectorRef) error {
	return xerrors.Errorf("not supported at this layer") // happens in localworker
}

func GetRequiredPadding(oldLength abi.PaddedPieceSize, newPieceLength abi.PaddedPieceSize) ([]abi.PaddedPieceSize, abi.PaddedPieceSize) {

	padPieces := make([]abi.PaddedPieceSize, 0)

	toFill := uint64(-oldLength % newPieceLength)

	n := bits.OnesCount64(toFill)
	var sum abi.PaddedPieceSize
	for i := 0; i < n; i++ {
		next := bits.TrailingZeros64(toFill)
		psize := uint64(1) << uint(next)
		toFill ^= psize

		padded := abi.PaddedPieceSize(psize)
		padPieces = append(padPieces, padded)
		sum += padded
	}

	return padPieces, sum
}

func GenerateUnsealedCID(proofType abi.RegisteredSealProof, pieces []abi.PieceInfo) (cid.Cid, error) {
	ssize, err := proofType.SectorSize()
	if err != nil {
		return cid.Undef, err
	}

	pssize := abi.PaddedPieceSize(ssize)
	allPieces := make([]abi.PieceInfo, 0, len(pieces))
	if len(pieces) == 0 {
		allPieces = append(allPieces, abi.PieceInfo{
			Size:     pssize,
			PieceCID: zerocomm.ZeroPieceCommitment(pssize.Unpadded()),
		})
	} else {
		var sum abi.PaddedPieceSize

		padTo := func(pads []abi.PaddedPieceSize) {
			for _, p := range pads {
				allPieces = append(allPieces, abi.PieceInfo{
					Size:     p,
					PieceCID: zerocomm.ZeroPieceCommitment(p.Unpadded()),
				})

				sum += p
			}
		}

		for _, p := range pieces {
			ps, _ := GetRequiredPadding(sum, p.Size)
			padTo(ps)

			allPieces = append(allPieces, p)
			sum += p.Size
		}

		ps, _ := GetRequiredPadding(sum, pssize)
		padTo(ps)
	}

	return ffi.GenerateUnsealedCID(proofType, allPieces)
}

type Commit2Request struct {
	SectorID   abi.SectorID
	Commit1Out storage.Commit1Out
}
type Commit2Response struct {
	SectorID abi.SectorID
	Proof    storage.Proof
}

func RequestCommit2(sector abi.SectorID, phase1Out storage.Commit1Out, c2Address string) (storage.Proof, error) {
	request := Commit2Request{}
	request.SectorID = abi.SectorID{
		Miner:  abi.ActorID(sector.Miner),
		Number: abi.SectorNumber(sector.Number),
	}
	request.Commit1Out = phase1Out

	b, err := json.Marshal(request)
	if err != nil {
		log.Errorf("json format error: %+v", err)
		return nil, err
	}

	zBuf := new(bytes.Buffer)
	zw := gzip.NewWriter(zBuf)
	if _, err = zw.Write(b); err != nil {
		log.Errorf("commit2 gzip failed, error: %+v", err)
	}

	zw.Flush()
	zw.Close()

	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s/proxy/commit2", c2Address), zBuf)
	if err != nil {
		log.Errorf("http new request err: %+v", err)
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json;charset=utf-8")
	req.Header.Set("Content-Encoding", "gzip")

	log.Info("Send commit2 task to extern server and wait for response ......")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Error("do request err: ", err)
		return nil, err
	}

	if resp.StatusCode != 200 {
		log.Errorf("http client do code: %d err: %+v", resp.StatusCode, err)
		return nil, err
	}
	defer resp.Body.Close()

	result, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("ioutil read all err: %+v", err)
		return nil, err
	}

	response := Commit2Response{}
	err = json.Unmarshal(result, &response)
	if err != nil {
		log.Errorf("json format len: %d error: %+v", len(result), err)
		return nil, err
	}

	log.Info("Receive commit2 result success......")

	return response.Proof, nil
}
