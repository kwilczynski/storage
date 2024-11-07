package chunked

import (
	archivetar "archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containers/storage/pkg/chunked/internal"
	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	digest "github.com/opencontainers/go-digest"
	"github.com/vbatts/tar-split/archive/tar"
)

const (
	// maxTocSize is the maximum size of a blob that we will attempt to process.
	// It is used to prevent DoS attacks from layers that embed a very large TOC file.
	maxTocSize = (1 << 20) * 50
)

var typesToTar = map[string]byte{
	TypeReg:     tar.TypeReg,
	TypeLink:    tar.TypeLink,
	TypeChar:    tar.TypeChar,
	TypeBlock:   tar.TypeBlock,
	TypeDir:     tar.TypeDir,
	TypeFifo:    tar.TypeFifo,
	TypeSymlink: tar.TypeSymlink,
}

func typeToTarType(t string) (byte, error) {
	r, found := typesToTar[t]
	if !found {
		return 0, fmt.Errorf("unknown type: %v", t)
	}
	return r, nil
}

func isZstdChunkedFrameMagic(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	return bytes.Equal(internal.ZstdChunkedFrameMagic, data[:8])
}

func readEstargzChunkedManifest(blobStream ImageSourceSeekable, blobSize int64, annotations map[string]string) ([]byte, int64, error) {
	// information on the format here https://github.com/containerd/stargz-snapshotter/blob/main/docs/stargz-estargz.md
	footerSize := int64(51)
	if blobSize <= footerSize {
		return nil, 0, errors.New("blob too small")
	}

	footer := make([]byte, footerSize)
	streamsOrErrors, err := getBlobAt(blobStream, ImageSourceChunk{Offset: uint64(blobSize - footerSize), Length: uint64(footerSize)})
	if err != nil {
		return nil, 0, err
	}

	for soe := range streamsOrErrors {
		if soe.stream != nil {
			_, err = io.ReadFull(soe.stream, footer)
			_ = soe.stream.Close()
		}
		if soe.err != nil && err == nil {
			err = soe.err
		}
	}

	/* Read the ToC offset:
	   - 10 bytes  gzip header
	   - 2  bytes  XLEN (length of Extra field) = 26 (4 bytes header + 16 hex digits + len("STARGZ"))
	   - 2  bytes  Extra: SI1 = 'S', SI2 = 'G'
	   - 2  bytes  Extra: LEN = 22 (16 hex digits + len("STARGZ"))
	   - 22 bytes  Extra: subfield = fmt.Sprintf("%016xSTARGZ", offsetOfTOC)
	   - 5  bytes  flate header: BFINAL = 1(last block), BTYPE = 0(non-compressed block), LEN = 0
	   - 8  bytes  gzip footer
	*/
	tocOffset, err := strconv.ParseInt(string(footer[16:16+22-6]), 16, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("parse ToC offset: %w", err)
	}

	size := int64(blobSize - footerSize - tocOffset)
	// set a reasonable limit
	if size > maxTocSize {
		return nil, 0, errors.New("manifest too big")
	}

	streamsOrErrors, err = getBlobAt(blobStream, ImageSourceChunk{Offset: uint64(tocOffset), Length: uint64(size)})
	if err != nil {
		return nil, 0, err
	}

	var manifestUncompressed []byte

	for soe := range streamsOrErrors {
		if soe.stream != nil {
			err1 := func() error {
				defer soe.stream.Close()

				r, err := pgzip.NewReader(soe.stream)
				if err != nil {
					return err
				}
				defer r.Close()

				aTar := archivetar.NewReader(r)

				header, err := aTar.Next()
				if err != nil {
					return err
				}
				// set a reasonable limit
				if header.Size > maxTocSize {
					return errors.New("manifest too big")
				}

				manifestUncompressed = make([]byte, header.Size)
				if _, err := io.ReadFull(aTar, manifestUncompressed); err != nil {
					return err
				}
				return nil
			}()
			if err == nil {
				err = err1
			}
		} else if err == nil {
			err = soe.err
		}
	}
	if manifestUncompressed == nil {
		return nil, 0, errors.New("manifest not found")
	}

	manifestDigester := digest.Canonical.Digester()
	manifestChecksum := manifestDigester.Hash()
	if _, err := manifestChecksum.Write(manifestUncompressed); err != nil {
		return nil, 0, err
	}

	d, err := digest.Parse(annotations[estargz.TOCJSONDigestAnnotation])
	if err != nil {
		return nil, 0, err
	}
	if manifestDigester.Digest() != d {
		return nil, 0, errors.New("invalid manifest checksum")
	}

	return manifestUncompressed, tocOffset, nil
}

// readZstdChunkedManifest reads the zstd:chunked manifest from the seekable stream blobStream.  The blob total size must
// be specified.
// This function uses the io.github.containers.zstd-chunked. annotations when specified.
func readZstdChunkedManifest(blobStream ImageSourceSeekable, blobSize int64, annotations map[string]string) (_ []byte, _ []byte, _ int64, retErr error) {
	footerSize := int64(internal.FooterSizeSupported)
	if blobSize <= footerSize {
		return nil, nil, 0, errors.New("blob too small")
	}

	manifestChecksumAnnotation := annotations[internal.ManifestChecksumKey]
	if manifestChecksumAnnotation == "" {
		return nil, nil, 0, fmt.Errorf("manifest checksum annotation %q not found", internal.ManifestChecksumKey)
	}

	var offset, length, lengthUncompressed, manifestType uint64

	var offsetTarSplit, lengthTarSplit, lengthUncompressedTarSplit uint64
	tarSplitChecksumAnnotation := ""

	if offsetMetadata := annotations[internal.ManifestInfoKey]; offsetMetadata != "" {
		if _, err := fmt.Sscanf(offsetMetadata, "%d:%d:%d:%d", &offset, &length, &lengthUncompressed, &manifestType); err != nil {
			return nil, nil, 0, err
		}

		if tarSplitInfoKeyAnnotation, found := annotations[internal.TarSplitInfoKey]; found {
			if _, err := fmt.Sscanf(tarSplitInfoKeyAnnotation, "%d:%d:%d", &offsetTarSplit, &lengthTarSplit, &lengthUncompressedTarSplit); err != nil {
				return nil, nil, 0, err
			}
			tarSplitChecksumAnnotation = annotations[internal.TarSplitChecksumKey]
		}
	} else {
		chunk := ImageSourceChunk{
			Offset: uint64(blobSize - footerSize),
			Length: uint64(footerSize),
		}
		parts, errs, err := blobStream.GetBlobAt([]ImageSourceChunk{chunk})
		if err != nil {
			return nil, nil, 0, err
		}
		var reader io.ReadCloser
		select {
		case r := <-parts:
			reader = r
		case err := <-errs:
			return nil, nil, 0, err
		}
		footer := make([]byte, footerSize)
		if _, err := io.ReadFull(reader, footer); err != nil {
			return nil, nil, 0, err
		}

		offset = binary.LittleEndian.Uint64(footer[0:8])
		length = binary.LittleEndian.Uint64(footer[8:16])
		lengthUncompressed = binary.LittleEndian.Uint64(footer[16:24])
		manifestType = binary.LittleEndian.Uint64(footer[24:32])
		if !isZstdChunkedFrameMagic(footer[48:56]) {
			return nil, nil, 0, errors.New("invalid magic number")
		}
	}

	if manifestType != internal.ManifestTypeCRFS {
		return nil, nil, 0, errors.New("invalid manifest type")
	}

	// set a reasonable limit
	if length > maxTocSize {
		return nil, nil, 0, errors.New("manifest too big")
	}
	if lengthUncompressed > maxTocSize {
		return nil, nil, 0, errors.New("manifest too big")
	}

	chunk := ImageSourceChunk{
		Offset: offset,
		Length: length,
	}

	chunks := []ImageSourceChunk{chunk}

	if offsetTarSplit > 0 {
		chunkTarSplit := ImageSourceChunk{
			Offset: offsetTarSplit,
			Length: lengthTarSplit,
		}
		chunks = append(chunks, chunkTarSplit)
	}

	streamsOrErrors, err := getBlobAt(blobStream, chunks...)
	if err != nil {
		return nil, nil, 0, err
	}

	defer func() {
		err := ensureAllBlobsDone(streamsOrErrors)
		if retErr == nil {
			retErr = err
		}
	}()

	readBlob := func(len uint64) ([]byte, error) {
		soe, ok := <-streamsOrErrors
		if !ok {
			return nil, errors.New("stream closed")
		}
		if soe.err != nil {
			return nil, soe.err
		}
		defer soe.stream.Close()

		blob := make([]byte, len)
		if _, err := io.ReadFull(soe.stream, blob); err != nil {
			return nil, err
		}
		return blob, nil
	}

	manifest, err := readBlob(length)
	if err != nil {
		return nil, nil, 0, err
	}

	decodedBlob, err := decodeAndValidateBlob(manifest, lengthUncompressed, manifestChecksumAnnotation)
	if err != nil {
		return nil, nil, 0, err
	}
	decodedTarSplit := []byte{}
	if offsetTarSplit > 0 {
		tarSplit, err := readBlob(lengthTarSplit)
		if err != nil {
			return nil, nil, 0, err
		}

		decodedTarSplit, err = decodeAndValidateBlob(tarSplit, lengthUncompressedTarSplit, tarSplitChecksumAnnotation)
		if err != nil {
			return nil, nil, 0, err
		}
	}
	return decodedBlob, decodedTarSplit, int64(offset), err
}

func decodeAndValidateBlob(blob []byte, lengthUncompressed uint64, expectedUncompressedChecksum string) ([]byte, error) {
	d, err := digest.Parse(expectedUncompressedChecksum)
	if err != nil {
		return nil, err
	}

	blobDigester := d.Algorithm().Digester()
	blobChecksum := blobDigester.Hash()
	if _, err := blobChecksum.Write(blob); err != nil {
		return nil, err
	}
	if blobDigester.Digest() != d {
		return nil, fmt.Errorf("invalid blob checksum, expected checksum %s, got %s", d, blobDigester.Digest())
	}

	decoder, err := zstd.NewReader(nil) //nolint:contextcheck
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	b := make([]byte, 0, lengthUncompressed)
	return decoder.DecodeAll(blob, b)
}
