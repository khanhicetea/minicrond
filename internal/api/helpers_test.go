package api

func (g *readGate) stats() (inflight int, bytes int64, waiting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight, g.bytes, g.waiting
}
