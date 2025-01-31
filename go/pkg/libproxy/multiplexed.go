package libproxy

import (
	"bufio"
	"bytes"
	"container/ring"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const defaultWindowSize = 65536

type windowState struct {
	current uint64
	allowed uint64
	max     uint64
}

func (w *windowState) String() string {
	return fmt.Sprintf("current %d, allowed %d, max %d", w.current, w.allowed, w.max)
}

func (w *windowState) size() int {
	return int(w.allowed - w.current)
}

func (w *windowState) isAlmostClosed() bool {
	return w.size() < int(w.max/2)
}

func (w *windowState) advance() {
	w.allowed = w.current + w.max
}

type channel struct {
	m             sync.Mutex
	c             *sync.Cond
	multiplexer   *multiplexer
	destination   Destination
	ID            uint32
	read          *windowState
	write         *windowState
	readPipe      *bufferedPipe
	closeReceived bool
	closeSent     bool
	// initially 2 (sender + receiver), protected by the multiplexer
	refCount                     int
	shutdownSent                 bool
	writeDeadline                time.Time
	testAllowDataAfterCloseWrite bool
}

func (c *channel) String() string {
	c.m.Lock()
	defer c.m.Unlock()
	closeReceived := ""
	if c.closeReceived {
		closeReceived = "closeReceived "
	}
	closeSent := ""
	if c.closeSent {
		closeSent = "closeSent "
	}
	shutdownSent := ""
	if c.shutdownSent {
		shutdownSent = "shutdownSent "
	}
	return fmt.Sprintf("ID %d -> %s %s%s%s", c.ID, c.destination.String(), closeReceived, closeSent, shutdownSent)
}

// newChannel registers a channel through the multiplexer
func newChannel(multiplexer *multiplexer, ID uint32, d Destination) *channel {
	readPipe := newBufferedPipe()
	c := &channel{
		multiplexer: multiplexer,
		destination: d,
		ID:          ID,
		read: &windowState{
			max: defaultWindowSize,
		},
		write: &windowState{
			max: defaultWindowSize,
		},
		readPipe: readPipe,
		refCount: 2,
	}
	c.c = sync.NewCond(&c.m)
	return c
}

func (c *channel) sendWindowUpdate() error {
	c.m.Lock()
	c.read.advance()
	seq := c.read.allowed
	c.m.Unlock()
	return c.multiplexer.send(NewWindow(c.ID, seq), nil)
}

func (c *channel) recvWindowUpdate(seq uint64) {
	c.m.Lock()
	c.write.allowed = seq
	// net.Conn says: Multiple goroutines may invoke methods on a Conn simultaneously.
	// Therefore there can be multiple goroutines blocked in Write, so when the window opens we should wake them all up.
	c.c.Broadcast()
	c.m.Unlock()
}

func (c *channel) Read(p []byte) (int, error) {
	n, err := c.readPipe.Read(p)
	c.m.Lock()
	c.read.current = c.read.current + uint64(n)
	needUpdate := c.read.isAlmostClosed()
	c.m.Unlock()
	if needUpdate {
		c.sendWindowUpdate()
	}
	return n, err
}

// for unit testing only
func (c *channel) setTestAllowDataAfterCloseWrite() {
	c.testAllowDataAfterCloseWrite = true
}

func (c *channel) Write(p []byte) (int, error) {
	c.m.Lock()
	defer c.m.Unlock()
	written := 0
	for {
		if len(p) == 0 {
			return written, nil
		}
		if c.closeReceived || c.closeSent || (c.shutdownSent && !c.testAllowDataAfterCloseWrite) {
			return written, io.EOF
		}
		if c.write.size() > 0 {
			// Some window space is available
			toWrite := c.write.size()
			if toWrite > len(p) {
				toWrite = len(p)
			}
			// Advance the window before dropping the lock in case another Write() appears and sees the same available space
			c.write.current = c.write.current + uint64(toWrite)

			// Don't block holding the metadata mutex.
			c.m.Unlock()
			err := c.multiplexer.send(NewData(c.ID, uint32(toWrite)), p[0:toWrite])
			c.m.Lock()

			if err != nil {
				return written, err
			}
			p = p[toWrite:]
			written = written + toWrite
			continue
		}

		// If the client has set a deadline then create a timer:
		var (
			timer   *time.Timer
			timeOut bool
		)
		if !c.writeDeadline.IsZero() {
			timer = time.AfterFunc(time.Until(c.writeDeadline), func() {
				c.m.Lock()
				defer c.m.Unlock()
				timeOut = true
				c.c.Broadcast()
			})
		}

		// Wait for the write window to be increased or a timeout
		c.c.Wait()

		if timer != nil {
			timer.Stop()
		}
		if timeOut {
			return written, &errTimeout{}
		}
	}
}

func (c *channel) Close() error {
	// Avoid a Write() racing with us and sending after we Close()
	// Avoid sending Close twice
	c.m.Lock()
	alreadyClosed := c.closeSent
	c.closeSent = true
	c.m.Unlock()

	if alreadyClosed {
		return nil
	}
	if err := c.multiplexer.send(NewClose(c.ID), nil); err != nil {
		return err
	}
	c.m.Lock()
	defer c.m.Unlock()
	c.c.Broadcast()

	c.multiplexer.decrChannelRef(c.ID)

	return nil
}

func (c *channel) CloseRead() error {
	return c.readPipe.CloseWrite()
}

func (c *channel) CloseWrite() error {
	// Avoid a Write() racing with us and sending after we Close()
	// Avoid sending Shutdown twice
	c.m.Lock()
	alreadyShutdown := c.shutdownSent || c.closeSent
	c.shutdownSent = true
	c.m.Unlock()

	if alreadyShutdown {
		return nil
	}
	if err := c.multiplexer.send(NewShutdown(c.ID), nil); err != nil {
		return err
	}
	c.m.Lock()
	defer c.m.Unlock()
	c.c.Broadcast()
	return nil
}

func (c *channel) recvClose() {
	c.m.Lock()
	defer c.m.Unlock()
	c.closeReceived = true
	c.c.Broadcast()
}

// SetReadBuffer sets the size of the operating system's receive buffer associated with the connection.
// See similar function https://pkg.go.dev/net#IPConn.SetReadBuffer
func (c *channel) SetReadBuffer(bytes uint) error {
	c.m.Lock()
	defer c.m.Unlock()
	c.read.max = uint64(bytes)
	// Will take effect on next window update
	return nil
}

// SetWriteBuffer sets the size of the operating system's write buffer associated with the connection.
// See similar function https://pkg.go.dev/net#IPConn.SetWriteBuffer
func (c *channel) SetWriteBuffer(bytes uint) error {
	c.m.Lock()
	defer c.m.Unlock()
	c.write.max = uint64(bytes)
	// Will take effect on next window update
	return nil
}

func (c *channel) SetReadDeadline(timeout time.Time) error {
	return c.readPipe.SetReadDeadline(timeout)
}

func (c *channel) SetWriteDeadline(timeout time.Time) error {
	c.m.Lock()
	defer c.m.Unlock()
	c.writeDeadline = timeout
	c.c.Broadcast()
	return nil
}

func (c *channel) SetDeadline(timeout time.Time) error {
	if err := c.SetReadDeadline(timeout); err != nil {
		return err
	}
	return c.SetWriteDeadline(timeout)
}

func (c *channel) RemoteAddr() net.Addr {
	return c
}

func (c *channel) LocalAddr() net.Addr {
	return c.RemoteAddr() // There is no local address
}

func (c *channel) Network() string {
	return "channel"
}

const (
	eventSend  = 0
	eventRecv  = 1
	eventOpen  = 2
	eventClose = 3
)

type event struct {
	eventType   int
	frame       *Frame      // for eventSend and eventRecv
	id          uint32      // for eventOpen and eventClose
	destination Destination // for eventOpen and eventClose
}

func (e *event) String() string {
	switch e.eventType {
	case eventSend:
		return fmt.Sprintf("send  %s", e.frame.String())
	case eventRecv:
		return fmt.Sprintf("recv  %s", e.frame.String())
	case eventOpen:
		return fmt.Sprintf("open  %d -> %s", e.id, e.destination)
	case eventClose:
		return fmt.Sprintf("close %d -> %s", e.id, e.destination)
	default:
		return "unknown trace event"
	}
}

// Multiplexer muxes and demuxes sub-connections over a single connection
type Multiplexer interface {
	Run()            // Run the multiplexer (otherwise Dial, Accept will not work)
	IsRunning() bool // IsRunning is true if the multiplexer is running normally, false if it has failed

	Dial(d Destination) (MultiplexedConn, error)    // Dial a remote Destination
	Accept() (MultiplexedConn, *Destination, error) // Accept a connection from a remote Destination

	Close() error // Close the multiplexer

	DumpState(w io.Writer) // WriteState dumps debug state to the writer
}

// MultiplexedConn is a sub-connection within a single multiplexed connection.
type MultiplexedConn interface {
	Conn
	SetReadBuffer(uint) error  // SetReadBuffer sets the maximum read buffer size
	SetWriteBuffer(uint) error // SetWriteBuffer sets the maximum write buffer size
}

type multiplexer struct {
	label             string
	conn              io.Closer
	connR             io.Reader // with buffering
	connW             *bufio.Writer
	writeMutex        sync.Mutex // hold when writing on the channel
	channels          map[uint32]*channel
	nextChannelID     uint32
	metadataMutex     sync.Mutex // hold when reading/modifying this structure
	pendingAccept     []*channel // incoming connections
	acceptCond        *sync.Cond
	isRunning         bool
	events            *ring.Ring // log of packetEvents
	eventsM           sync.Mutex
	allocateBackwards bool
}

// NewMultiplexer constructs a multiplexer from a channel
func NewMultiplexer(label string, conn io.ReadWriteCloser, allocateBackwards bool) (Multiplexer, error) {
	channels := make(map[uint32]*channel)
	connR := bufio.NewReader(conn)
	connW := bufio.NewWriter(conn)
	events := ring.New(500)

	// Perform the handshake
	localH := &handshake{}

	g := &errgroup.Group{}

	g.Go(func() error { return localH.Write(conn) })
	g.Go(func() error {
		_, err := unmarshalHandshake(connR)
		return err
	})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	nextId := uint32(0)
	if allocateBackwards {
		nextId = ^nextId
	}
	m := &multiplexer{
		label:             label,
		conn:              conn,
		connR:             connR,
		connW:             connW,
		channels:          channels,
		nextChannelID:     nextId,
		events:            events,
		allocateBackwards: allocateBackwards,
	}
	m.acceptCond = sync.NewCond(&m.metadataMutex)
	return m, nil
}

// Close the underlying transport.
func (m *multiplexer) Close() error {
	m.metadataMutex.Lock()
	m.isRunning = false
	m.metadataMutex.Unlock()
	return m.conn.Close()
}

func (m *multiplexer) appendEvent(e *event) {
	m.eventsM.Lock()
	defer m.eventsM.Unlock()
	m.events.Value = e
	m.events = m.events.Next()
}

// send a frame (header) plus optional payload. If this call fails then the multiplexed connection will be desynchronised.
func (m *multiplexer) send(f *Frame, payload []byte) error {
	m.writeMutex.Lock()
	defer m.writeMutex.Unlock()
	m.appendEvent(&event{eventType: eventSend, frame: f})

	if err := f.Write(m.connW); err != nil {
		return fmt.Errorf("writing frame %s: %w", f, err)
	}
	if n, err := m.connW.Write(payload); err != nil || n != len(payload) {
		return fmt.Errorf("writing frame %s payload length %d: %d, %w", f, len(payload), n, err)
	}
	if err := m.connW.Flush(); err != nil {
		return fmt.Errorf("flushing frame %s: %w", f, err)
	}
	return nil
}

func (m *multiplexer) findFreeChannelID() uint32 {
	// the metadataMutex is already held
	if m.allocateBackwards {
		id := m.nextChannelID
		for {
			if _, ok := m.channels[id]; !ok {
				m.nextChannelID = id - 1
				return id
			}
			id--
		}
	}
	id := m.nextChannelID
	for {
		if _, ok := m.channels[id]; !ok {
			m.nextChannelID = id + 1
			return id
		}
		id++
	}
}

func (m *multiplexer) decrChannelRef(ID uint32) {
	m.metadataMutex.Lock()
	defer m.metadataMutex.Unlock()
	if channel, ok := m.channels[ID]; ok {
		if channel.refCount == 1 {
			m.appendEvent(&event{eventType: eventClose, id: ID, destination: channel.destination})
			delete(m.channels, ID)
			return
		}
		channel.refCount = channel.refCount - 1
	}
}

// Dial opens a connection to the given destination
func (m *multiplexer) Dial(d Destination) (MultiplexedConn, error) {
	m.metadataMutex.Lock()
	if !m.isRunning {
		m.metadataMutex.Unlock()
		return nil, errors.New("connection refused")
	}
	id := m.findFreeChannelID()
	channel := newChannel(m, id, d)
	m.channels[id] = channel
	m.metadataMutex.Unlock()

	if err := m.send(NewOpen(id, d), nil); err != nil {
		return nil, err
	}
	if err := channel.sendWindowUpdate(); err != nil {
		return nil, err
	}
	if d.Proto == UDP {
		// remove encapsulation
		return newUDPConn(channel), nil
	}
	return channel, nil
}

var ErrNotRunning = errors.New("multiplexer is not running")

// Accept returns the next client connection
func (m *multiplexer) Accept() (MultiplexedConn, *Destination, error) {
	first, err := m.nextPendingAccept()
	if err != nil {
		return nil, nil, err
	}
	if err := first.sendWindowUpdate(); err != nil {
		return nil, nil, err
	}
	if first.destination.Proto == UDP {
		return newUDPConn(first), &first.destination, nil
	}
	return first, &first.destination, nil
}

func (m *multiplexer) nextPendingAccept() (*channel, error) {
	m.metadataMutex.Lock()
	defer m.metadataMutex.Unlock()
	for {
		if !m.isRunning {
			return nil, ErrNotRunning
		}
		if len(m.pendingAccept) > 0 {
			first := m.pendingAccept[0]
			m.pendingAccept = m.pendingAccept[1:]
			return first, nil
		}
		m.acceptCond.Wait()
	}
}

// Run starts handling the requests from the other side
func (m *multiplexer) Run() {
	m.metadataMutex.Lock()
	m.isRunning = true
	m.metadataMutex.Unlock()
	go func() {
		err := m.run()
		m.metadataMutex.Lock()
		expected := err == io.EOF || !m.isRunning
		m.metadataMutex.Unlock()
		if expected {
			// This is expected when the data connection is broken
			log.Infof("disconnected data connection: multiplexer is offline")
		} else if err != nil {
			log.Printf("Multiplexer main loop failed with %v", err)
			m.DumpState(log.Writer())
		}
		m.metadataMutex.Lock()
		m.isRunning = false
		m.acceptCond.Broadcast()
		var channels []*channel
		for _, channel := range m.channels {
			channels = append(channels, channel)
		}
		m.metadataMutex.Unlock()

		// close all open channels
		for _, channel := range channels {
			// this will unblock waiting Read calls
			channel.readPipe.closeWriteNoErr()
			// this will unblock waiting Write calls
			channel.recvClose()
			m.decrChannelRef(channel.ID)
		}

	}()
}

// DumpState writes internal multiplexer state
func (m *multiplexer) DumpState(w io.Writer) {
	m.eventsM.Lock()
	io.WriteString(w, "Event trace:\n")
	m.events.Do(func(p interface{}) {
		if e, ok := p.(*event); ok {
			io.WriteString(w, e.String())
			io.WriteString(w, "\n")
		}
	})
	m.eventsM.Unlock()

	var channels []*channel
	m.metadataMutex.Lock()
	for _, c := range m.channels {
		channels = append(channels, c)
	}
	m.metadataMutex.Unlock()

	io.WriteString(w, "Active channels:\n")
	for _, c := range channels {
		io.WriteString(w, c.String())
		io.WriteString(w, "\n")
	}
	io.WriteString(w, "End of state dump\n")
}

// IsRunning returns whether the multiplexer is running or not
func (m *multiplexer) IsRunning() bool {
	m.metadataMutex.Lock()
	defer m.metadataMutex.Unlock()
	return m.isRunning
}

func (m *multiplexer) run() error {
	for {
		f, err := unmarshalFrame(m.connR)
		if err != nil {
			return err
		}
		m.appendEvent(&event{eventType: eventRecv, frame: f})
		switch payload := f.Payload().(type) {
		case *OpenFrame:
			o, err := f.Open()
			if err != nil {
				return fmt.Errorf("Failed to unmarshal open command: %v", err)
			}
			switch o.Connection {
			case Dedicated:
				return fmt.Errorf("Dedicated connections are not implemented yet")
			case Multiplexed:
				m.metadataMutex.Lock()
				channel := newChannel(m, f.ID, o.Destination)
				m.channels[f.ID] = channel
				m.pendingAccept = append(m.pendingAccept, channel)
				m.acceptCond.Signal()
				m.metadataMutex.Unlock()
				m.appendEvent(&event{eventType: eventOpen, id: f.ID, destination: o.Destination})
			}
		case *WindowFrame:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id %s", f.String())
			}
			channel.recvWindowUpdate(payload.seq)
		case *DataFrame:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id: %s", f.String())
			}
			// We don't use a direct io.Copy or io.CopyN to the readPipe because if they get
			// EOF on Write, they will drop the data in the buffer and we don't know how big
			// it was so we can't avoid desychronising the stream.
			// We trust the clients not to write more than a Window size.
			var buf bytes.Buffer
			if _, err := io.CopyN(&buf, m.connR, int64(payload.payloadlen)); err != nil {
				return fmt.Errorf("Failed to read payload of %d bytes: %s", payload.payloadlen, f.String())
			}
			if n, err := io.Copy(channel.readPipe, &buf); err != nil {
				// err must be io.EOF
				log.Printf("Discarded %d bytes from %s", int64(payload.payloadlen)-n, f.String())
				// A confused client could send a DataFrame after a ShutdownFrame or CloseFrame.
				// The stream is not desychronised so we can keep going.
			}
		case *ShutdownFrame:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id: %s", f.String())
			}
			channel.readPipe.closeWriteNoErr()
		case *CloseFrame:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id: %s", f.String())
			}
			// this will unblock waiting Read calls
			channel.readPipe.closeWriteNoErr()
			// this will unblock waiting Write calls
			channel.recvClose()
			m.decrChannelRef(channel.ID)
		default:
			return fmt.Errorf("Unknown command type: %v", f)
		}
	}
}
