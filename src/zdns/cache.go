/* ZDNS Copyright 2024 Regents of the University of Michigan
*
* Licensed under the Apache License, Version 2.0 (the "License"); you may not
* use this file except in compliance with the License. You may obtain a copy
* of the License at http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
* implied. See the License for the specific language governing
* permissions and limitations under the License.
 */

package zdns

import (
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/zmap/dns"

	"github.com/zmap/zdns/src/internal/cachehash"
	"github.com/zmap/zdns/src/internal/util"
)

type IsCached bool

type TimedAnswer struct {
	Answer    interface{}
	ExpiresAt time.Time
}

type CachedKey struct {
	Question    Question
	NameServer  string // optional
	IsAuthority bool
}

type CachedResult struct {
	Answers map[interface{}]TimedAnswer
}

type Cache struct {
	IterativeCache cachehash.ShardedCacheHash
	//Hits           atomic.Uint64
	//Misses         atomic.Uint64
	//Adds           atomic.Uint64
}

func (s *Cache) Init(cacheSize int) {
	s.IterativeCache.Init(cacheSize, 4096)
}

func (s *Cache) VerboseLog(depth int, args ...interface{}) {
	log.Debug(makeVerbosePrefix(depth), args)
}

func (s *Cache) AddCachedAnswer(answer interface{}, ns *NameServer, depth int) {
	//s.Adds.Add(1)
	a, ok := answer.(Answer)
	if !ok {
		// we can't cache this entry because we have no idea what to name it
		return
	}
	q := questionFromAnswer(a)

	// only cache records that can help prevent future iteration: A(AAA), NS, (C|D)NAME.
	// This will prevent some entries that will never help future iteration (e.g., PTR)
	// from causing unnecessary cache evictions.
	// TODO: this is overly broad right now and will unnecessarily cache some leaf A/AAAA records. However,
	// it's a lot of work to understand _why_ we're doing a specific lookup and this will still help
	// in other cases, e.g., PTR lookups
	if !(q.Type == dns.TypeA || q.Type == dns.TypeAAAA || q.Type == dns.TypeNS || q.Type == dns.TypeDNAME || q.Type == dns.TypeCNAME) {
		return
	}
	expiresAt := time.Now().Add(time.Duration(a.TTL) * time.Second)
	ca := CachedResult{}
	ca.Answers = make(map[interface{}]TimedAnswer)
	cacheKey := CachedKey{q, "", false}
	if ns != nil {
		cacheKey.NameServer = ns.String()
	}
	s.IterativeCache.Lock(cacheKey)
	defer s.IterativeCache.Unlock(cacheKey)
	// don't bother to move this to the top of the linked list. we're going
	// to add this record back in momentarily and that will take care of this
	i, ok := s.IterativeCache.GetNoMove(cacheKey)
	if ok {
		// record found, check type on interface
		ca, ok = i.(CachedResult)
		if !ok {
			log.Panic("unable to cast cached result")
		}
	}
	// we have an existing record. Let's add this answer to it.
	ta := TimedAnswer{
		Answer:    answer,
		ExpiresAt: expiresAt}
	ca.Answers[a] = ta
	s.IterativeCache.Add(cacheKey, ca)
	s.VerboseLog(depth+1, "Upsert cached answer ", q, " ", ca)
}

func (s *Cache) GetCachedResult(q Question, ns *NameServer, depth int) (SingleQueryResult, bool) {
	var retv SingleQueryResult
	cacheKey := CachedKey{q, "", false}
	if ns != nil {
		cacheKey.NameServer = ns.String()
		s.VerboseLog(depth+1, "Cache request for: ", q.Name, " (", q.Type, ") @", cacheKey.NameServer)
	} else {
		s.VerboseLog(depth+1, "Cache request for: ", q.Name, " (", q.Type, ")")
	}
	s.IterativeCache.Lock(cacheKey)
	unres, ok := s.IterativeCache.Get(cacheKey)
	if !ok { // nothing found
		//s.Misses.Add(1)
		s.VerboseLog(depth+2, "-> no entry found in cache for ", q.Name)
		s.IterativeCache.Unlock(cacheKey)
		return retv, false
	}
	//s.Hits.Add(1)
	retv.Authorities = make([]interface{}, 0)
	retv.Answers = make([]interface{}, 0)
	retv.Additional = make([]interface{}, 0)
	cachedRes, ok := unres.(CachedResult)
	if !ok {
		log.Panic("unable to cast cached result for ", q.Name)
	}
	// great we have a result. let's go through the entries and build a result. In the process, throw away anything
	// that's expired
	now := time.Now()
	for k, cachedAnswer := range cachedRes.Answers {
		if cachedAnswer.ExpiresAt.Before(now) {
			// if we have a write lock, we can perform the necessary actions
			// and then write this back to the cache. However, if we don't,
			// we need to start this process over with a write lock
			s.VerboseLog(depth+2, "Expiring cache entry ", k)
			delete(cachedRes.Answers, k)
		} else {
			// this result is valid. append it to the SingleQueryResult we're going to hand to the user
			retv.Answers = append(retv.Answers, cachedAnswer.Answer)
		}
	}
	s.IterativeCache.Unlock(cacheKey)
	// Don't return an empty response.
	if len(retv.Answers) == 0 && len(retv.Authorities) == 0 && len(retv.Additional) == 0 {
		s.VerboseLog(depth+2, "-> no entry found in cache, after expiration for ", q.Name)
		var emptyRetv SingleQueryResult
		return emptyRetv, false
	}
	if ns != nil {
		retv.Resolver = ns.String()
	}

	s.VerboseLog(depth+2, "Cache hit for ", q.Name, ": ", retv)
	return retv, true
}

func (s *Cache) SafeAddCachedAnswer(a interface{}, ns *NameServer, layer, debugType string, depth int) {
	ans, ok := a.(Answer)
	if !ok {
		s.VerboseLog(depth+1, "unable to cast ", debugType, ": ", layer, ": ", a)
		return
	}
	if ok, _ := nameIsBeneath(ans.Name, layer); !ok {
		log.Info("detected poison ", debugType, ": ", ans.Name, "(", ans.Type, "): ", layer, ": ", a)
		return
	}
	s.AddCachedAnswer(a, ns, depth)
}

func (s *Cache) SafeAddLayerNameServers(layer string, result SingleQueryResult, ns *NameServer, depth int, cacheNonAuthoritativeAns bool) {
	authsAndAdditionals := util.Concat(result.Authorities, result.Additional)
	// build a map of TimedAnswers to add to cache
	timedAns := make(map[interface{}]TimedAnswer, len(authsAndAdditionals))
	for _, a := range authsAndAdditionals {
		castAns, ok := a.(Answer)
		if !ok {
			log.Info("unable to cast authority in layer name servers: ", layer, ": ", a)
			continue
		}
		//if ok, _ = nameIsBeneath(castAns.Name, layer); !ok {
		//	log.Info("detected poison in adding nameserver authorities: ", castAns.Name, "(", castAns.Type, "): ", layer, ": ", castAns)
		//	return
		//}
		if castAns.RrType != dns.TypeNS && castAns.RrType != dns.TypeA && castAns.RrType != dns.TypeAAAA {
			log.Info("ignoring unexpected RRType in layer name servers: ", layer, ": ", castAns)
			continue
		}
		timedAns[a] = TimedAnswer{
			Answer:    a,
			ExpiresAt: time.Now().Add(time.Duration(a.(Answer).TTL) * time.Second),
		}
	}
	cacheKey := CachedKey{Question: Question{Name: layer, Type: dns.TypeNS}, IsAuthority: true}
	s.IterativeCache.Lock(cacheKey)
	defer s.IterativeCache.Unlock(cacheKey)
	//s.Adds.Add(1)
	s.IterativeCache.Add(cacheKey, CachedResult{Answers: timedAns})
}

func (s *Cache) GetLayerNameServers(name string) (SingleQueryResult, bool) {
	res := SingleQueryResult{}
	res.Answers = make([]interface{}, 0)
	res.Authorities = make([]interface{}, 0)
	res.Additional = make([]interface{}, 0)
	cacheKey := CachedKey{Question: Question{Name: name, Type: dns.TypeNS}, IsAuthority: true}
	s.IterativeCache.Lock(cacheKey)
	defer s.IterativeCache.Unlock(cacheKey)
	unres, ok := s.IterativeCache.Get(cacheKey)
	if !ok {
		//s.Misses.Add(1)
		return SingleQueryResult{}, false
	}
	//s.Hits.Add(1)
	cachedRes, ok := unres.(CachedResult)
	if !ok {
		log.Panic("unable to cast cached result for ", name)
	}
	for _, cachedAnswer := range cachedRes.Answers {
		// check expiration
		if cachedAnswer.ExpiresAt.Before(time.Now()) {
			delete(cachedRes.Answers, cachedAnswer.Answer)
			continue
		}
		castAns, ok := cachedAnswer.Answer.(Answer)
		if !ok {
			log.Panic("unable to cast cached answer for ", name)
		}
		if castAns.RrType == dns.TypeNS {
			res.Authorities = append(res.Authorities, castAns)
		} else if castAns.RrType == dns.TypeA || castAns.RrType == dns.TypeAAAA {
			res.Additional = append(res.Additional, castAns)
		} else {
			log.Info("ignoring unexpected RRType in layer name servers: ", name, ": ", castAns)
		}
	}
	return res, true
}

func (s *Cache) CacheUpdate(layer string, result SingleQueryResult, ns *NameServer, depth int, cacheNonAuthoritativeAns bool) {
	for _, a := range result.Additional {
		s.SafeAddCachedAnswer(a, ns, layer, "additional", depth)
	}
	for _, a := range result.Authorities {
		s.SafeAddCachedAnswer(a, ns, layer, "authority", depth)
	}
	if result.Flags.Authoritative || cacheNonAuthoritativeAns {
		for _, a := range result.Answers {
			s.SafeAddCachedAnswer(a, ns, layer, "answer", depth)
		}
	}
}
