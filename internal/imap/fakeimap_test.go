package imap

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"katchup/internal/account"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
)

// fakeIMAP is an in-process IMAP server (go-imap's memory backend) for
// exercising the real sync path. It can report an arbitrary UIDVALIDITY and
// serve chosen messages with an empty body, the way a provider occasionally
// returns a message it can't render.
type fakeIMAP struct {
	addr string
	mbox *memory.Mailbox

	mu          sync.Mutex
	uidValidity uint32
	emptyBody   map[uint32]bool
	bodyFetches map[uint32]int
}

func newFakeIMAP(t *testing.T) *fakeIMAP {
	t.Helper()
	be := memory.New()
	u, err := be.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	mb, err := u.GetMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIMAP{
		mbox:        mb.(*memory.Mailbox),
		uidValidity: 1,
		emptyBody:   map[uint32]bool{},
		bodyFetches: map[uint32]int{},
	}
	f.mbox.Messages = nil

	srv := server.New(&fakeBackend{Backend: be, f: f})
	srv.AllowInsecureAuth = true
	srv.ErrorLog = nopLogger{}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	f.addr = l.Addr().String()
	return f
}

// add appends messages with the given UIDs (ascending) to INBOX.
func (f *fakeIMAP) add(uids ...uint32) {
	for _, uid := range uids {
		body := fmt.Sprintf("From: a@example.com\r\nTo: b@example.com\r\nSubject: m%d\r\n"+
			"Message-ID: <m%d@example.com>\r\nDate: Wed, 11 May 2016 14:31:59 +0000\r\n\r\nbody %d\r\n", uid, uid, uid)
		f.mbox.Messages = append(f.mbox.Messages, &memory.Message{
			Uid: uid, Date: time.Date(2016, 5, 11, 14, 31, 59, 0, time.UTC),
			Size: uint32(len(body)), Body: []byte(body),
		})
	}
}

func (f *fakeIMAP) setUIDValidity(v uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uidValidity = v
}

func (f *fakeIMAP) setEmptyBody(uid uint32, empty bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emptyBody[uid] = empty
}

func (f *fakeIMAP) fetches(uid uint32) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodyFetches[uid]
}

// useFakeIMAP points the syncer's connections at f.
func useFakeIMAP(s *Syncer, f *fakeIMAP) {
	s.dial = func(account.Account) (*client.Client, error) {
		c, err := client.Dial(f.addr)
		if err != nil {
			return nil, err
		}
		c.Timeout = commandTimeout
		if err := c.Login("username", "password"); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}
func (nopLogger) Println(...interface{})        {}

type fakeBackend struct {
	*memory.Backend
	f *fakeIMAP
}

func (b *fakeBackend) Login(ci *imap.ConnInfo, username, password string) (backend.User, error) {
	u, err := b.Backend.Login(ci, username, password)
	if err != nil {
		return nil, err
	}
	return &fakeUser{User: u, f: b.f}, nil
}

type fakeUser struct {
	backend.User
	f *fakeIMAP
}

func (u *fakeUser) GetMailbox(name string) (backend.Mailbox, error) {
	mb, err := u.User.GetMailbox(name)
	if err != nil {
		return nil, err
	}
	return &fakeMailbox{Mailbox: mb, f: u.f}, nil
}

type fakeMailbox struct {
	backend.Mailbox
	f *fakeIMAP
}

func (m *fakeMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	st, err := m.Mailbox.Status(items)
	if st != nil {
		m.f.mu.Lock()
		st.UidValidity = m.f.uidValidity
		m.f.mu.Unlock()
	}
	return st, err
}

func (m *fakeMailbox) ListMessages(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	inner := make(chan *imap.Message)
	done := make(chan error, 1)
	go func() { done <- m.Mailbox.ListMessages(uid, seqSet, items, inner) }()
	for msg := range inner {
		m.f.mu.Lock()
		for section := range msg.Body {
			m.f.bodyFetches[msg.Uid]++
			if m.f.emptyBody[msg.Uid] {
				msg.Body[section] = bytes.NewBuffer(nil)
			}
		}
		m.f.mu.Unlock()
		ch <- msg
	}
	close(ch)
	return <-done
}
