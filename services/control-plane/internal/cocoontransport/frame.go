package cocoontransport

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	frameBootstrap byte = iota + 1
	frameProviderStdin
	frameProviderStdinEOF
	frameProviderStdout
	frameProviderStderr
	frameProviderExit
	frameTunnelOpen
	frameTunnelGuestData
	frameTunnelGuestClose
	frameTunnelHostData
	frameTunnelHostClose
)

const (
	maximumFrameBytes = 4 << 20
	streamChunkBytes  = 32 << 10
)

type frame struct {
	kind    byte
	payload []byte
}

type frameWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *frameWriter) write(kind byte, payload []byte) error {
	if len(payload) > maximumFrameBytes {
		return errors.New("Cocoon transport frame is oversized")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	header := [5]byte{kind}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.writer.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.writer.Write(payload)
	return err
}

func readFrame(reader io.Reader) (frame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return frame{}, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maximumFrameBytes {
		return frame{}, errors.New("Cocoon transport frame is oversized")
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return frame{}, err
	}
	return frame{kind: header[0], payload: payload}, nil
}

func tunnelPayload(id uint64, payload []byte) []byte {
	result := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint64(result, id)
	copy(result[8:], payload)
	return result
}

func decodeTunnelPayload(payload []byte) (uint64, []byte, error) {
	if len(payload) < 8 {
		return 0, nil, fmt.Errorf("Cocoon transport tunnel frame is truncated")
	}
	id := binary.BigEndian.Uint64(payload)
	if id == 0 {
		return 0, nil, errors.New("Cocoon transport tunnel identity is invalid")
	}
	return id, payload[8:], nil
}
