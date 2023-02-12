package libp2pwebrtc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/p2p/transport/webrtc/internal/async"
	pb "github.com/libp2p/go-libp2p/p2p/transport/webrtc/pb"
	"github.com/libp2p/go-msgio/pbio"
	"github.com/pion/datachannel"
)

type (
	webRTCStreamReader struct {
		stream *webRTCStream

		state *async.MutexExec[*webRTCStreamReaderState]

		deadline async.MutexGetterSetter[time.Time]

		closeOnce sync.Once
	}

	webRTCStreamReaderState struct {
		Reader pbio.Reader
		Buffer []byte
	}
)

// Read from the underlying datachannel. This also
// process sctp control messages such as DCEP, which is
// handled internally by pion, and stream closure which
// is signaled by `Read` on the datachannel returning
// io.EOF.
func (r *webRTCStreamReader) Read(b []byte) (int, error) {
	var (
		readErr  error
		read     int
		finished bool
	)
	for !finished && readErr == nil {
		if r.stream.isClosed() {
			return 0, io.ErrClosedPipe
		}

		readDeadline, hasReadDeadline := r.getReadDeadline()
		if hasReadDeadline {
			// check if deadline exceeded
			if readDeadline.Before(time.Now()) {
				if err, found := r.stream.closeErr.Get(); found {
					log.Debugf("[1] deadline exceeded: closeErr: %v", err)
				} else {
					log.Debug("[1] deadline exceeded: no closeErr")
				}
				return 0, os.ErrDeadlineExceeded
			}
		}

		readErr = r.state.Exec(func(state *webRTCStreamReaderState) error {
			read = copy(b, state.Buffer)
			state.Buffer = state.Buffer[read:]
			remaining := len(state.Buffer)

			if remaining == 0 && !r.stream.stateHandler.AllowRead() {
				closeErr, _ := r.stream.closeErr.Get()
				if closeErr != nil {
					log.Debugf("[2] stream closed: %v", closeErr)
					return closeErr
				}
				log.Debug("[2] stream empty")
				return io.EOF
			}

			if read > 0 || read == len(b) {
				finished = true
				return nil
			}

			// read from datachannel
			var msg pb.Message
			readErr = state.Reader.ReadMsg(&msg)
			if readErr != nil {
				// This case occurs when the remote node goes away
				// without writing a FIN message
				if errors.Is(readErr, io.EOF) {
					r.stream.Reset()
					return io.ErrClosedPipe
				}
				if errors.Is(readErr, os.ErrDeadlineExceeded) {
					// if the stream has been force closed or force reset
					// using SetReadDeadline, we check if closeErr was set.
					closeErr, _ := r.stream.closeErr.Get()
					log.Debugf("closing stream, checking error: %v closeErr: %v", readErr, closeErr)
					if closeErr != nil {
						return closeErr
					}
				}
				return readErr
			}

			// append incoming data to read buffer
			if r.stream.stateHandler.AllowRead() && msg.Message != nil {
				state.Buffer = append(state.Buffer, msg.GetMessage()...)
			}

			// process any flags on the message
			if msg.Flag != nil {
				r.stream.processIncomingFlag(msg.GetFlag())
			}
			return nil
		})
	}

	return read, readErr
}

func (r *webRTCStreamReader) SetReadDeadline(t time.Time) error {
	r.deadline.Set(t)
	return r.stream.rwc.(*datachannel.DataChannel).SetReadDeadline(t)
}

func (r *webRTCStreamReader) getReadDeadline() (time.Time, bool) {
	return r.deadline.Get()
}

func (r *webRTCStreamReader) CloseRead() error {
	if r.stream.isClosed() {
		return nil
	}
	var err error
	r.closeOnce.Do(func() {
		err = r.stream.writer.writer.Exec(func(writer pbio.Writer) error {
			return writer.WriteMsg(&pb.Message{Flag: pb.Message_STOP_SENDING.Enum()})
		})
		if err != nil {
			log.Debug("could not write STOP_SENDING message")
			err = fmt.Errorf("could not close stream for reading: %w", err)
			return
		}
		if r.stream.stateHandler.CloseRead() == stateClosed {
			r.stream.close(false, true)
		}
	})
	return err
}