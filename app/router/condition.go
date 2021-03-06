package router

import (
	"context"
	"strings"
	"sync"
	"time"

	"v2ray.com/core/app/dispatcher"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/protocol"
	"v2ray.com/core/common/strmatcher"
	"v2ray.com/core/proxy"
)

type Condition interface {
	Apply(ctx context.Context) bool
}

type ConditionChan []Condition

func NewConditionChan() *ConditionChan {
	var condChan ConditionChan = make([]Condition, 0, 8)
	return &condChan
}

func (v *ConditionChan) Add(cond Condition) *ConditionChan {
	*v = append(*v, cond)
	return v
}

func (v *ConditionChan) Apply(ctx context.Context) bool {
	for _, cond := range *v {
		if !cond.Apply(ctx) {
			return false
		}
	}
	return true
}

func (v *ConditionChan) Len() int {
	return len(*v)
}

type AnyCondition []Condition

func NewAnyCondition() *AnyCondition {
	var anyCond AnyCondition = make([]Condition, 0, 8)
	return &anyCond
}

func (v *AnyCondition) Add(cond Condition) *AnyCondition {
	*v = append(*v, cond)
	return v
}

func (v *AnyCondition) Apply(ctx context.Context) bool {
	for _, cond := range *v {
		if cond.Apply(ctx) {
			return true
		}
	}
	return false
}

func (v *AnyCondition) Len() int {
	return len(*v)
}

type timedResult struct {
	timestamp time.Time
	result    bool
}

type CachableDomainMatcher struct {
	sync.Mutex
	matchers *strmatcher.MatcherGroup
	cache    map[string]timedResult
	lastScan time.Time
}

func NewCachableDomainMatcher() *CachableDomainMatcher {
	return &CachableDomainMatcher{
		matchers: strmatcher.NewMatcherGroup(),
		cache:    make(map[string]timedResult, 512),
	}
}

var matcherTypeMap = map[Domain_Type]strmatcher.Type{
	Domain_Plain:  strmatcher.Substr,
	Domain_Regex:  strmatcher.Regex,
	Domain_Domain: strmatcher.Domain,
}

func (m *CachableDomainMatcher) Add(domain *Domain) error {
	matcherType, f := matcherTypeMap[domain.Type]
	if !f {
		return newError("unsupported domain type", domain.Type)
	}

	matcher, err := matcherType.New(domain.Value)
	if err != nil {
		return newError("failed to create domain matcher").Base(err)
	}

	m.matchers.Add(matcher)
	return nil
}

func (m *CachableDomainMatcher) applyInternal(domain string) bool {
	return m.matchers.Match(domain) > 0
}

type cacheResult int

const (
	cacheMiss cacheResult = iota
	cacheHitTrue
	cacheHitFalse
)

func (m *CachableDomainMatcher) findInCache(domain string) cacheResult {
	m.Lock()
	defer m.Unlock()

	r, f := m.cache[domain]
	if !f {
		return cacheMiss
	}
	r.timestamp = time.Now()
	m.cache[domain] = r

	if r.result {
		return cacheHitTrue
	}
	return cacheHitFalse
}

func (m *CachableDomainMatcher) ApplyDomain(domain string) bool {
	if m.matchers.Size() < 64 {
		return m.applyInternal(domain)
	}

	cr := m.findInCache(domain)

	if cr == cacheHitTrue {
		return true
	}

	if cr == cacheHitFalse {
		return false
	}

	r := m.applyInternal(domain)
	m.Lock()
	defer m.Unlock()

	m.cache[domain] = timedResult{
		result:    r,
		timestamp: time.Now(),
	}

	now := time.Now()
	if len(m.cache) > 256 && now.Sub(m.lastScan)/time.Second > 5 {
		now := time.Now()

		for k, v := range m.cache {
			if now.Sub(v.timestamp)/time.Second > 60 {
				delete(m.cache, k)
			}
		}

		m.lastScan = now
	}

	return r
}

func (m *CachableDomainMatcher) Apply(ctx context.Context) bool {
	dest, ok := proxy.TargetFromContext(ctx)
	if !ok {
		return false
	}

	if !dest.Address.Family().IsDomain() {
		return false
	}
	return m.ApplyDomain(dest.Address.Domain())
}

type CIDRMatcher struct {
	cidr     *net.IPNet
	onSource bool
}

func NewCIDRMatcher(ip []byte, mask uint32, onSource bool) (*CIDRMatcher, error) {
	cidr := &net.IPNet{
		IP:   net.IP(ip),
		Mask: net.CIDRMask(int(mask), len(ip)*8),
	}
	return &CIDRMatcher{
		cidr:     cidr,
		onSource: onSource,
	}, nil
}

func (v *CIDRMatcher) Apply(ctx context.Context) bool {
	ips := make([]net.IP, 0, 4)
	if resolver, ok := proxy.ResolvedIPsFromContext(ctx); ok {
		resolvedIPs := resolver.Resolve()
		for _, rip := range resolvedIPs {
			if !rip.Family().IsIPv6() {
				continue
			}
			ips = append(ips, rip.IP())
		}
	}

	var dest net.Destination
	var ok bool
	if v.onSource {
		dest, ok = proxy.SourceFromContext(ctx)
	} else {
		dest, ok = proxy.TargetFromContext(ctx)
	}

	if ok && dest.Address.Family().IsIPv6() {
		ips = append(ips, dest.Address.IP())
	}

	for _, ip := range ips {
		if v.cidr.Contains(ip) {
			return true
		}
	}
	return false
}

type IPv4Matcher struct {
	ipv4net  *net.IPNetTable
	onSource bool
}

func NewIPv4Matcher(ipnet *net.IPNetTable, onSource bool) *IPv4Matcher {
	return &IPv4Matcher{
		ipv4net:  ipnet,
		onSource: onSource,
	}
}

func (v *IPv4Matcher) Apply(ctx context.Context) bool {
	ips := make([]net.IP, 0, 4)
	if resolver, ok := proxy.ResolvedIPsFromContext(ctx); ok {
		resolvedIPs := resolver.Resolve()
		for _, rip := range resolvedIPs {
			if !rip.Family().IsIPv4() {
				continue
			}
			ips = append(ips, rip.IP())
		}
	}

	var dest net.Destination
	var ok bool
	if v.onSource {
		dest, ok = proxy.SourceFromContext(ctx)
	} else {
		dest, ok = proxy.TargetFromContext(ctx)
	}

	if ok && dest.Address.Family().IsIPv4() {
		ips = append(ips, dest.Address.IP())
	}

	for _, ip := range ips {
		if v.ipv4net.Contains(ip) {
			return true
		}
	}
	return false
}

type PortMatcher struct {
	port net.PortRange
}

func NewPortMatcher(portRange net.PortRange) *PortMatcher {
	return &PortMatcher{
		port: portRange,
	}
}

func (v *PortMatcher) Apply(ctx context.Context) bool {
	dest, ok := proxy.TargetFromContext(ctx)
	if !ok {
		return false
	}
	return v.port.Contains(dest.Port)
}

type NetworkMatcher struct {
	network *net.NetworkList
}

func NewNetworkMatcher(network *net.NetworkList) *NetworkMatcher {
	return &NetworkMatcher{
		network: network,
	}
}

func (v *NetworkMatcher) Apply(ctx context.Context) bool {
	dest, ok := proxy.TargetFromContext(ctx)
	if !ok {
		return false
	}
	return v.network.HasNetwork(dest.Network)
}

type UserMatcher struct {
	user []string
}

func NewUserMatcher(users []string) *UserMatcher {
	usersCopy := make([]string, 0, len(users))
	for _, user := range users {
		if len(user) > 0 {
			usersCopy = append(usersCopy, user)
		}
	}
	return &UserMatcher{
		user: usersCopy,
	}
}

func (v *UserMatcher) Apply(ctx context.Context) bool {
	user := protocol.UserFromContext(ctx)
	if user == nil {
		return false
	}
	for _, u := range v.user {
		if u == user.Email {
			return true
		}
	}
	return false
}

type InboundTagMatcher struct {
	tags []string
}

func NewInboundTagMatcher(tags []string) *InboundTagMatcher {
	tagsCopy := make([]string, 0, len(tags))
	for _, tag := range tags {
		if len(tag) > 0 {
			tagsCopy = append(tagsCopy, tag)
		}
	}
	return &InboundTagMatcher{
		tags: tagsCopy,
	}
}

func (v *InboundTagMatcher) Apply(ctx context.Context) bool {
	tag, ok := proxy.InboundTagFromContext(ctx)
	if !ok {
		return false
	}

	for _, t := range v.tags {
		if t == tag {
			return true
		}
	}
	return false
}

type ProtocolMatcher struct {
	protocols []string
}

func NewProtocolMatcher(protocols []string) *ProtocolMatcher {
	pCopy := make([]string, 0, len(protocols))

	for _, p := range protocols {
		if len(p) > 0 {
			pCopy = append(pCopy, p)
		}
	}

	return &ProtocolMatcher{
		protocols: pCopy,
	}
}

func (m *ProtocolMatcher) Apply(ctx context.Context) bool {
	result := dispatcher.SniffingResultFromContext(ctx)

	if result == nil {
		return false
	}

	protocol := result.Protocol()
	for _, p := range m.protocols {
		if strings.HasPrefix(protocol, p) {
			return true
		}
	}

	return false
}
