package scheduler

import (
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/mesos"
	"github.com/mesosphere/kubernetes-mesos/queue"
)

const (
	offerListenerMaxAge      = 5 // max number of times we'll attempt to fit an offer to a listener before requiring them to re-register themselves
	deferredDeclineTtlFactor = 2 // this factor, multiplied by the offer ttl, determines how long to wait before attempting to decline previously claimed offers that were subsequently deleted, then released. see offerStorage.Delete
)

type OfferFilter func(*mesos.Offer) bool

type OfferRegistry interface {
	// Initialize the instance, spawning necessary housekeeping go routines.
	Init()
	Add([]*mesos.Offer)

	// Listen for arriving offers that are acceptable to the filter, sending
	// a signal on (by closing) the returned channel. A listener will only
	// ever be notified once, if at all.
	Listen(id string, f OfferFilter) <-chan empty

	// invoked when offers are rescinded or expired
	Delete(string)

	Get(offerId string) (PerishableOffer, bool)

	Walk(Walker) error

	// invalidate one or all (when offerId="") offers; offers are not declined,
	// but are simply flagged as expired in the offer history
	Invalidate(offerId string)
}

// callback that is invoked during a walk through a series of live offers,
// returning with stop=true (or err != nil) if the walk should stop permaturely.
type Walker func(offer PerishableOffer) (stop bool, err error)

type OfferRegistryConfig struct {
	declineOffer  func(offerId string) error
	ttl           time.Duration // determines a perishable offer's expiration deadline: now+ttl
	lingerTtl     time.Duration // if zero, offers will not linger in the FIFO past their expiration deadline
	listenerDelay time.Duration // specifies the sleep time between offer listener notifications
}

type offerStorage struct {
	OfferRegistryConfig
	offers    *cache.FIFO       // collection of PerishableOffer, both live and expired
	listeners *cache.FIFO       // collection of *offerListener
	delayed   *queue.DelayQueue // deadline-oriented offer-event queue
}

type liveOffer struct {
	*mesos.Offer
	expiration time.Time
	acquired   int32 // 1 = acquired, 0 = free
}

type expiredOffer struct {
	id       string
	deadline time.Time
}

type PerishableOffer interface {
	queue.Delayed
	// returns true if this offer has expired
	HasExpired() bool
	// if not yet expired, return mesos offer details; otherwise nil
	Details() *mesos.Offer
	// mark this offer as acquired, returning true if it was previously unacquired. thread-safe.
	Acquire() bool
	// mark this offer as un-acquired. thread-safe.
	Release()
	// expire or delete this offer from storage
	age(s *offerStorage)
}

func (e *expiredOffer) HasExpired() bool {
	return true
}

func (e *expiredOffer) Details() *mesos.Offer {
	return nil
}

func (e *expiredOffer) Acquire() bool {
	return false
}

func (e *expiredOffer) Release() {}

func (e *expiredOffer) age(s *offerStorage) {
	log.V(3).Infof("Delete lingering offer: %v", e.id)
	s.offers.Delete(e.id)
}

// return the time left to linger
func (e *expiredOffer) GetDelay() time.Duration {
	return e.deadline.Sub(time.Now())
}

func (to *liveOffer) HasExpired() bool {
	return time.Now().After(to.expiration)
}

func (to *liveOffer) Details() *mesos.Offer {
	return to.Offer
}

func (to *liveOffer) Acquire() bool {
	return atomic.CompareAndSwapInt32(&to.acquired, 0, 1)
}

func (to *liveOffer) Release() {
	atomic.CompareAndSwapInt32(&to.acquired, 1, 0)
}

func (to *liveOffer) age(s *offerStorage) {
	s.Delete(to.Offer.Id.GetValue())
}

// return the time remaining before the offer expires
func (to *liveOffer) GetDelay() time.Duration {
	return to.expiration.Sub(time.Now())
}

func CreateOfferRegistry(c OfferRegistryConfig) OfferRegistry {
	return &offerStorage{c, cache.NewFIFO(), cache.NewFIFO(), queue.NewDelayQueue()}
}

func (s *offerStorage) Add(offers []*mesos.Offer) {
	now := time.Now()
	for _, offer := range offers {
		offerId := offer.Id.GetValue()
		log.V(3).Infof("Receiving offer %v", offerId)
		timed := &liveOffer{offer, now.Add(s.ttl), 0}
		s.offers.Add(offerId, timed)
		s.delayed.Add(timed)
	}
}

// delete an offer from storage, meaning that we expire it
func (s *offerStorage) Delete(offerId string) {
	if offer, ok := s.Get(offerId); ok {
		log.V(3).Infof("Deleting offer %v", offerId)
		// attempt to block others from consuming the offer. if it's already been
		// claimed and is not yet lingering then don't decline it - just mark it as
		// expired in the history: allow a prior claimant to attempt to launch with it
		myoffer := offer.Acquire()
		if offer.Details() != nil {
			if myoffer {
				log.V(3).Infof("Declining offer %v", offerId)
				if err := s.declineOffer(offerId); err != nil {
					log.Warningf("Failed to decline offer %v: %v", offerId, err)
				}
			} else {
				// some pod has acquired this and may attempt to launch a task with it
				// failed schedule/launch attempts are requried to Release() any claims on the offer
				go func() {
					// TODO(jdef): not sure what a good value is here. the goal is to provide a
					// launchTasks (driver) operation enough time to complete so that we don't end
					// up declining an offer that we're actually attempting to use.
					time.Sleep(deferredDeclineTtlFactor * s.ttl)

					// at this point the offer is in one of five states:
					// a) permanently deleted: expired due to timeout
					// b) permanently deleted: expired due to having been rescinded
					// c) lingering: expired due to timeout
					// d) lingering: expired due to having been rescinded
					// e) claimed: task launched and it using resources from this offer
					// we want to **avoid** declining an offer that's claimed: attempt to acquire
					if offer.Acquire() {
						// previously claimed offer was released, perhaps due to a launch
						// failure, so we should attempt to decline
						if err := s.declineOffer(offerId); err != nil {
							log.Warningf("Failed to decline (previously claimed) offer %v: %v", offerId, err)
						}
					}
				}()
			}
		}
		s.expireOffer(offer)
	} // else, ignore offers not in the history
}

// expire all known, live offers
func (s *offerStorage) Invalidate(offerId string) {
	if offerId != "" {
		s.invalidateOne(offerId)
		return
	}
	obj := s.offers.List()
	for _, o := range obj {
		offer, ok := o.(PerishableOffer)
		if !ok {
			log.Errorf("Expected perishable offer, not %v", o)
			continue
		}
		offer.Acquire() // attempt to block others from using it
		s.expireOffer(offer)
		// don't decline, we already know that it's an invalid offer
	}
}

func (s *offerStorage) invalidateOne(offerId string) {
	if offer, ok := s.Get(offerId); ok {
		offer.Acquire() // attempt to block others from using it
		s.expireOffer(offer)
		// don't decline, we already know that it's an invalid offer
	}
}

// Walk the collection of offers. The walk stops either as indicated by the
// Walker or when the end of the offer list is reached. Expired offers are
// never passed to a Walker.
func (s *offerStorage) Walk(w Walker) error {
	for offerId := range s.offers.Contains() {
		offer, ok := s.Get(offerId)
		if !ok {
			// offer disappeared...
			continue
		}
		if offer.HasExpired() {
			// never pass expired offers to walkers
			continue
		}
		if stop, err := w(offer); err != nil {
			return err
		} else if stop {
			return nil
		}
	}
	return nil
}

func (s *offerStorage) expireOffer(offer PerishableOffer) {
	// the offer may or may not be expired due to TTL so check for details
	// since that's a more reliable determinant of lingering status
	if details := offer.Details(); details != nil {
		// recently expired, should linger
		offerId := details.Id.GetValue()
		log.V(3).Infof("Expiring offer %v", offerId)
		if s.lingerTtl > 0 {
			log.V(3).Infof("offer will linger: %v", offerId)
			expired := &expiredOffer{offerId, time.Now().Add(s.lingerTtl)}
			s.offers.Update(offerId, expired)
			s.delayed.Add(expired)
		} else {
			log.V(3).Infof("Permanently deleting offer %v", offerId)
			s.offers.Delete(offerId)
		}
	} // else, it's still lingering...
}

func (s *offerStorage) Get(id string) (PerishableOffer, bool) {
	if obj, ok := s.offers.Get(id); !ok {
		return nil, false
	} else {
		to, ok := obj.(PerishableOffer)
		if !ok {
			log.Errorf("invalid offer object in fifo '%v'", obj)
		}
		return to, ok
	}
}

type offerListener struct {
	id      string
	accepts OfferFilter
	notify  chan<- empty
	age     int
}

// register a listener for new offers, whom we'll notify upon receiving such.
// notification is delivered in the form of closing the channel, nothing is ever sent.
func (s *offerStorage) Listen(id string, f OfferFilter) <-chan empty {
	if f == nil {
		return nil
	}
	ch := make(chan empty)
	listen := &offerListener{
		id:      id,
		accepts: f,
		notify:  ch,
		age:     0,
	}
	log.V(3).Infof("Registering offer listener %s", listen.id)
	s.listeners.Add(id, listen)
	return ch
}

func (s *offerStorage) ageOffers() {
	offer, ok := s.delayed.Pop().(PerishableOffer)
	if !ok {
		log.Errorf("Expected PerishableOffer, not %v", offer)
		return
	}
	if details := offer.Details(); details != nil && !offer.HasExpired() {
		// live offer has not expired yet: timed out early
		// FWIW: early timeouts are more frequent when GOMAXPROCS is > 1
		s.delayed.Add(offer)
	} else {
		offer.age(s)
	}
}

func (s *offerStorage) notifyListeners() {
	var listen *offerListener
	var ok bool
	// get the next offer listener
	for {
		obj := s.listeners.Pop()
		if listen, ok = obj.(*offerListener); ok {
			break
		}
		log.Warningf("unexpected listener object %v", obj)
	}
	// notify if we find an acceptable offer
	for id := range s.offers.Contains() {
		var offer PerishableOffer
		if offer, ok = s.Get(id); !ok || offer.HasExpired() {
			continue
		}
		if listen.accepts(offer.Details()) {
			log.V(3).Infof("Notifying offer listener %s", listen.id)
			close(listen.notify)
			return
		}
	}
	// no interesting offers found, re-queue the listener
	listen.age++
	if listen.age < offerListenerMaxAge {
		// if the same listener has re-registered in the meantime we don't want to
		// destroy the newer listener channel. this is racy since a newer listener
		// can register between the Get() and Update(), but the consequences aren't
		// very dire - the listener merely has to wait their full backoff period.
		if _, ok := s.listeners.Get(listen.id); !ok {
			log.V(3).Infof("Re-registering offer listener %s", listen.id)
			s.listeners.Update(listen.id, listen)
		}
	} // else, you're gc'd
}

func (s *offerStorage) Init() {
	go util.Forever(s.ageOffers, 0)

	// to avoid a rush on offers, we add a short delay between each registered
	// listener, so as to allow the most recently notified listener a bit of time
	// to act on the offer.
	go util.Forever(s.notifyListeners, s.listenerDelay)
}
