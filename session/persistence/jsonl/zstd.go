// Zstandard frame container for the JSONL persistence backend (official
// zstd.ts): each durable batch — the creation header or one appended event
// flush — is one independently decodable, checksummed Zstandard frame
// appended to a concatenated-frame artifact. A structural scan locates
// complete frames and any torn final frame WITHOUT decompressing, so crash
// recovery truncates at the last frame boundary and the write-behind
// coordinator re-flushes the lost batch.
//
// Frame bytes are standard Zstandard (magic 0xFD2FB528, CRC enabled), so
// any conforming zstd implementation can decode the artifacts.
package jsonl

import (
	"encoding/binary"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// zstdMagic is the Zstandard frame magic number (little-endian).
const zstdMagic = 0xFD2FB528

// zstdFrameDecoder/encoder are process-shared: both are concurrency-safe
// and hold codec tables sized once.
var (
	zstdEncoder, _ = zstd.NewWriter(nil,
		zstd.WithEncoderCRC(true), // official CHECKSUM_OPTIONS: ZSTD_c_checksumFlag=1
		zstd.WithEncoderConcurrency(1))
	zstdDecoder, _ = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1))
)

// zstdFrameRange is the byte range of one structurally complete frame.
type zstdFrameRange struct {
	start int64
	end   int64
}

// zstdFrameScan is the structural scan result: complete frame ranges plus
// the start of an EOF-interrupted final frame, when one exists.
type zstdFrameScan struct {
	frames    []zstdFrameRange
	tornStart int64
	torn      bool
}

// scanZstdFrames locates complete frames without decompressing their
// blocks. Invalid complete structure rejects; EOF inside the final frame
// marks it torn for repair. Verbatim port of the official scanZstdFrames.
func scanZstdFrames(buffer []byte) (zstdFrameScan, error) {
	scan := zstdFrameScan{}
	offset := int64(0)
	for offset < int64(len(buffer)) {
		start := offset
		if int64(len(buffer))-offset < 4 {
			scan.tornStart, scan.torn = start, true
			return scan, nil
		}
		if binary.LittleEndian.Uint32(buffer[offset:offset+4]) != zstdMagic {
			return scan, fmt.Errorf("corrupt Zstandard session log: invalid frame magic at byte %d", offset)
		}
		offset += 4

		if offset == int64(len(buffer)) {
			scan.tornStart, scan.torn = start, true
			return scan, nil
		}
		descriptor := buffer[offset]
		offset++
		if descriptor&0x18 != 0 {
			return scan, fmt.Errorf("corrupt Zstandard session log: reserved frame-header bit at byte %d", offset-1)
		}
		contentSizeFlag := descriptor >> 6
		singleSegment := descriptor&0x20 != 0
		checksum := descriptor&0x04 != 0
		dictionaryFlag := descriptor & 0x03
		dictionaryBytes := int64(0)
		if dictionaryFlag == 3 {
			dictionaryBytes = 4
		} else {
			dictionaryBytes = int64(dictionaryFlag)
		}
		contentSizeBytes := int64(0)
		if contentSizeFlag == 0 {
			if singleSegment {
				contentSizeBytes = 1
			}
		} else {
			contentSizeBytes = 1 << contentSizeFlag
		}
		remainingHeaderBytes := int64(0)
		if !singleSegment {
			remainingHeaderBytes++
		}
		remainingHeaderBytes += dictionaryBytes + contentSizeBytes
		if int64(len(buffer))-offset < remainingHeaderBytes {
			scan.tornStart, scan.torn = start, true
			return scan, nil
		}
		offset += remainingHeaderBytes

		for {
			if int64(len(buffer))-offset < 3 {
				scan.tornStart, scan.torn = start, true
				return scan, nil
			}
			blockHeader := uint32(buffer[offset]) | uint32(buffer[offset+1])<<8 | uint32(buffer[offset+2])<<16
			offset += 3
			lastBlock := blockHeader&1 != 0
			blockType := (blockHeader >> 1) & 0x03
			blockSize := blockHeader >> 3
			if blockType == 0x03 {
				return scan, fmt.Errorf("corrupt Zstandard session log: reserved block type at byte %d", offset-3)
			}
			payloadBytes := int64(blockSize)
			if blockType == 0x01 {
				payloadBytes = 1
			}
			if int64(len(buffer))-offset < payloadBytes {
				scan.tornStart, scan.torn = start, true
				return scan, nil
			}
			offset += payloadBytes
			if lastBlock {
				break
			}
		}

		if checksum {
			if int64(len(buffer))-offset < 4 {
				scan.tornStart, scan.torn = start, true
				return scan, nil
			}
			offset += 4
		}
		scan.frames = append(scan.frames, zstdFrameRange{start: start, end: offset})
	}
	return scan, nil
}

// compressZstdFrame produces one independently decodable, checksummed frame
// over the input plaintext (EncodeAll emits exactly one complete frame).
func compressZstdFrame(input []byte) ([]byte, error) {
	return zstdEncoder.EncodeAll(input, nil), nil
}

// decodeZstdFrames decodes the concatenated complete frames of one
// container in source order (DecodeAll walks concatenated frames
// natively).
func decodeZstdFrames(container []byte) ([]byte, error) {
	return zstdDecoder.DecodeAll(container, nil)
}
