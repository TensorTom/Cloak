package multiplex

import (
	"errors"
	log "github.com/sirupsen/logrus"
	"net"
	"sync"
	"sync/atomic"
)

// switchboard is responsible for keeping the reference of TLS connections between client and server
type switchboard struct {
	session *Session

	*Valve

	// optimum is the connEnclave with the smallest sendQueue
	optimum atomic.Value // *connEnclave
	cesM    sync.RWMutex
	ces     []*connEnclave
}

func (sb *switchboard) getOptimum() *connEnclave {
	if i := sb.optimum.Load(); i == nil {
		return nil
	} else {
		return i.(*connEnclave)
	}
}

// Some data comes from a Stream to be sent through one of the many
// remoteConn, but which remoteConn should we use to send the data?
//
// In this case, we pick the remoteConn that has about the smallest sendQueue.
type connEnclave struct {
	remoteConn net.Conn
	sendQueue  uint32
}

func makeSwitchboard(sesh *Session, valve *Valve) *switchboard {
	// rates are uint64 because in the usermanager we want the bandwidth to be atomically
	// operated (so that the bandwidth can change on the fly).
	sb := &switchboard{
		session: sesh,
		Valve:   valve,
		ces:     []*connEnclave{},
	}
	return sb
}

var errNilOptimum error = errors.New("The optimal connection is nil")

func (sb *switchboard) send(data []byte) (int, error) {
	ce := sb.getOptimum()
	if ce == nil {
		return 0, errNilOptimum
	}
	atomic.AddUint32(&ce.sendQueue, uint32(len(data)))
	go sb.updateOptimum()
	n, err := ce.remoteConn.Write(data)
	if err != nil {
		return n, err
	}
	sb.txWait(n)
	sb.Valve.AddTx(int64(n))
	atomic.AddUint32(&ce.sendQueue, ^uint32(n-1))
	go sb.updateOptimum()
	return n, nil
}

func (sb *switchboard) updateOptimum() {
	currentOpti := sb.getOptimum()
	currentOptiQ := atomic.LoadUint32(&currentOpti.sendQueue)
	sb.cesM.RLock()
	for _, ce := range sb.ces {
		ceQ := atomic.LoadUint32(&ce.sendQueue)
		if ceQ < currentOptiQ {
			currentOpti = ce
			currentOptiQ = ceQ
		}
	}
	sb.cesM.RUnlock()
	sb.optimum.Store(currentOpti)
}

func (sb *switchboard) addConn(conn net.Conn) {
	var sendQueue uint32
	newCe := &connEnclave{
		remoteConn: conn,
		sendQueue:  sendQueue,
	}
	sb.cesM.Lock()
	sb.ces = append(sb.ces, newCe)
	sb.cesM.Unlock()
	sb.optimum.Store(newCe)
	go sb.deplex(newCe)
}

func (sb *switchboard) removeConn(closing *connEnclave) {
	sb.cesM.Lock()
	for i, ce := range sb.ces {
		if closing == ce {
			sb.ces = append(sb.ces[:i], sb.ces[i+1:]...)
			break
		}
	}
	if len(sb.ces) == 0 {
		sb.session.SetTerminalMsg("no underlying connection left")
		sb.session.Close()
	}
	sb.cesM.Unlock()
}

// actively triggered by session.Close()
func (sb *switchboard) closeAll() {
	sb.cesM.RLock()
	for _, ce := range sb.ces {
		ce.remoteConn.Close()
	}
	sb.cesM.RUnlock()
}

// deplex function costantly reads from a TCP connection, call Deobfs and distribute it
// to the corresponding stream
func (sb *switchboard) deplex(ce *connEnclave) {
	buf := make([]byte, 20480)
	for {
		n, err := sb.session.unitRead(ce.remoteConn, buf)
		sb.rxWait(n)
		sb.Valve.AddRx(int64(n))
		if err != nil {
			log.Tracef("a connection for session %v has closed: %v", sb.session.id, err)
			go ce.remoteConn.Close()
			sb.removeConn(ce)
			return
		}

		frame, err := sb.session.Deobfs(buf[:n])
		if err != nil {
			log.Debugf("Failed to decrypt a frame for session %v: %v", sb.session.id, err)
			continue
		}

		stream := sb.session.getStream(frame.StreamID, frame.Closing == 1)
		// if the frame is telling us to close a closed stream
		// (this happens when ss-server and ss-local closes the stream
		// simutaneously), we don't do anything
		if stream != nil {
			stream.writeNewFrame(frame)
		}
	}
}
