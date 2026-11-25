package main

import (
	"airouter/internal/config"
	"airouter/internal/crypto"
)

// enableConfiguredReplay signs Grok Build Chat replay only with a private
// instance secret. isDev covers the unset configuration path. A configured
// copy of the built-in development secret is also rejected. At-rest encryption
// stays on the cipher created by the caller.
func enableConfiguredReplay(cipher *crypto.Cipher, cfg config.Config, isDev bool) error {
	if cipher == nil || isDev {
		return nil
	}
	replay, ok := cfg.PrivateReplaySecret()
	if !ok {
		return nil
	}
	return cipher.EnableGrokBuildChatReplay(replay)
}
