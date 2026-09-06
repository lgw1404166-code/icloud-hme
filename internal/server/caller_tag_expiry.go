package server

import "time"

const pendingCallerExpiryTick = 30 * time.Second

// startPendingCallerExpiryWorker makes a caller allocation self-healing even
// when the caller never comes back to read the inbox. It only clears entries
// that were still pending after their five-minute confirmation window.
func (s *Server) startPendingCallerExpiryWorker() {
	if s.pendingCallerStop == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(pendingCallerExpiryTick)
		defer ticker.Stop()
		for {
			if _, err := s.mgr.ExpirePendingCallerAllocations(); err != nil {
				// The Manager persists atomically; a later tick can safely retry.
			}
			select {
			case <-s.pendingCallerStop:
				return
			case <-ticker.C:
			}
		}
	}()
}
