package cloud

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/lydakis/errand/internal/placement"
	"github.com/lydakis/errand/internal/proto"
)

// A Catalog lists offers that come and go, such as a cloud account's
// instance types with capacity right now. The broker lists it on a schedule
// and again before leasing from a listing that may be stale.
type Catalog interface {
	Offers(ctx context.Context) ([]Offer, error)
}

// catalogTimeout bounds one listing.
const catalogTimeout = 15 * time.Second

// refreshCatalog keeps the catalog listed, every CatalogRefresh, until the
// broker closes. A listing that fails leaves the last one in place.
func (b *Broker) refreshCatalog() {
	defer b.wg.Done()
	b.freshenCatalog()
	close(b.listed)
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(b.cfg.CatalogRefresh):
		}
		b.freshenCatalog()
	}
}

// freshenCatalog lists the catalog again unless it was listed within
// CatalogRefresh. Callers hold no lock; one listing runs at a time, and a
// caller that waited for another's takes its outcome, failed or not, instead
// of listing again.
func (b *Broker) freshenCatalog() {
	if b.cfg.Catalog == nil {
		return
	}
	asked := time.Now()
	b.catalogMu.Lock()
	defer b.catalogMu.Unlock()
	b.mu.Lock()
	fresh := !b.catalogAt.IsZero() && time.Since(b.catalogAt) < b.cfg.CatalogRefresh
	b.mu.Unlock()
	if fresh || b.catalogTried.After(asked) {
		return
	}
	defer func() { b.catalogTried = time.Now() }()
	ctx, cancel := context.WithTimeout(b.ctx, catalogTimeout)
	defer cancel()
	offers, err := b.cfg.Catalog.Offers(ctx)
	if err != nil {
		if b.ctx.Err() == nil {
			log.Printf("cloud catalog: %v", err)
		}
		return
	}
	var listed []Offer
	for _, o := range offers {
		// A catalog offer that shares a configured offer's name is hidden
		// by it, so configuration always wins.
		if _, configured := b.offers[o.Name]; configured || o.Name == "" || o.Provider == nil || o.IdleTimeout <= 0 || o.MaxLifetime <= 0 {
			continue
		}
		b.setUp(o.Provider)
		listed = append(listed, o)
	}
	sort.SliceStable(listed, func(i, j int) bool { return placement.CheaperOffer(listed[i].Offer(), listed[j].Offer()) })
	b.mu.Lock()
	b.catalog, b.catalogAt = listed, time.Now()
	b.mu.Unlock()
}

// offerFacts are the facts of the offer a launching lease was made from, or
// none when a catalog no longer lists it. The caller holds b.mu.
func (l *lease) offerFacts(b *Broker) (facts proto.Facts) {
	if o, ok := b.offerLocked(l.Offer); ok {
		return o.Facts
	}
	return facts
}
