package store

// GrokBuildChatMAC signs payload with the instance Chat-replay key.
// The key stays inside the cipher. ok is false when the store has no cipher.
func (s *Store) GrokBuildChatMAC(payload []byte) ([]byte, bool) {
	if s == nil || s.cipher == nil {
		return nil, false
	}
	return s.cipher.GrokBuildChatMAC(payload)
}

// VerifyGrokBuildChatMAC checks a Chat-replay MAC. A missing cipher fails closed.
func (s *Store) VerifyGrokBuildChatMAC(payload, mac []byte) bool {
	if s == nil || s.cipher == nil {
		return false
	}
	return s.cipher.VerifyGrokBuildChatMAC(payload, mac)
}

// CodexChatMAC signs payload with the Codex Chat-replay key.
func (s *Store) CodexChatMAC(payload []byte) ([]byte, bool) {
	if s == nil || s.cipher == nil {
		return nil, false
	}
	return s.cipher.CodexChatMAC(payload)
}

// VerifyCodexChatMAC checks a Codex Chat-replay MAC. A missing cipher fails closed.
func (s *Store) VerifyCodexChatMAC(payload, mac []byte) bool {
	if s == nil || s.cipher == nil {
		return false
	}
	return s.cipher.VerifyCodexChatMAC(payload, mac)
}
