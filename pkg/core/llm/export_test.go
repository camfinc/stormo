package llm

import "time"

// ExpireCatalog ages the cached catalog past its TTL: the next call refreshes it.
func (g *Gateway) ExpireCatalog() {
	g.mu.Lock()
	g.catalogAt = time.Time{}
	g.mu.Unlock()
}
