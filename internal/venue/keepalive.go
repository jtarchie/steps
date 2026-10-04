package venue

import (
	"time"

	"golang.org/x/crypto/ssh"
)

//nolint:gochecknoglobals // test seams for a liveness check measured in seconds
var (
	keepaliveInterval = 15 * time.Second
	keepaliveTimeout  = 15 * time.Second
)

// keepAlive closes client when the far end stops answering: a tunnel, SSM's above all, can sit silent forever after the machine behind it is gone, and every channel read waits with it. Any reply counts, since OpenSSH answers an unknown request with a refusal.
func keepAlive(client *ssh.Client) {
	// Read once, here: a goroutine that read the seams on every tick would race a later test that sets them.
	interval, timeout := keepaliveInterval, keepaliveTimeout
	ended := make(chan struct{})

	go func() {
		_ = client.Wait()

		close(ended)
	}()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ended:
				return
			case <-ticker.C:
			}

			answered := make(chan struct{})

			go func() {
				_, _, _ = client.SendRequest("keepalive@openssh.com", true, nil)

				close(answered)
			}()

			select {
			case <-answered:
			case <-ended:
				return
			case <-time.After(timeout):
				_ = client.Close()

				return
			}
		}
	}()
}
