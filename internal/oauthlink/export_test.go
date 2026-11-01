package oauthlink

import "testing"

// Test hooks for the external test package (settings_test.go).
type StubServer = stubServer

func NewStubForTest(t *testing.T) *StubServer { return newStub(t) }
func NewLinkerForTest(t *testing.T, s *StubServer) *Linker {
	l, _ := newLinker(t, s)
	return l
}
func StateOf(s *StubServer) string { s.mu.Lock(); defer s.mu.Unlock(); return s.par.Get("state") }

const BotDIDForTest = botDID
