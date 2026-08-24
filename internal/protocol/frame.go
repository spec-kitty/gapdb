package protocol

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/spec-kitty/gapdb/gapdb"
)

const framePrefixBytes = 4

func ReadFrame(reader io.Reader, maximum int) ([]byte, error) {
	if maximum <= 0 || maximum > gapdb.HardMaxFrameBytes {
		return nil, invalidProtocol("maximum_frame_bytes", "is outside the supported range", nil)
	}
	var prefix [framePrefixBytes]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, invalidProtocol("frame_length", "could not read the complete four-byte prefix", err)
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 {
		return nil, invalidProtocol("frame_length", "must be greater than zero", nil)
	}
	if uint64(length) > uint64(maximum) {
		return nil, frameTooLarge(int(length), maximum)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, invalidProtocol("frame_payload", "ended before the declared length", err)
	}
	return payload, nil
}

func WriteFrame(writer io.Writer, payload []byte, maximum int) error {
	if maximum <= 0 || maximum > gapdb.HardMaxFrameBytes {
		return invalidProtocol("maximum_frame_bytes", "is outside the supported range", nil)
	}
	if len(payload) == 0 {
		return invalidProtocol("frame_length", "must be greater than zero", nil)
	}
	if len(payload) > maximum || uint64(len(payload)) > math.MaxUint32 {
		return frameTooLarge(len(payload), maximum)
	}
	var prefix [framePrefixBytes]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	if err := writeAll(writer, prefix[:]); err != nil {
		return invalidProtocol("frame_length", "could not write the complete four-byte prefix", err)
	}
	if err := writeAll(writer, payload); err != nil {
		return invalidProtocol("frame_payload", "could not write the complete payload", err)
	}
	return nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if written < 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
