package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/data-preservation-programs/RetrievalBot/pkg/convert"
	"github.com/data-preservation-programs/RetrievalBot/pkg/requesterror"
	"github.com/data-preservation-programs/RetrievalBot/pkg/resources"
	"github.com/filecoin-project/go-state-types/abi"
	logging "github.com/ipfs/go-log/v2"
	"github.com/jellydator/ttlcache/v3"
	"github.com/multiformats/go-multiaddr"
	"github.com/pkg/errors"
	"golang.org/x/exp/slices"
)

//nolint:gochecknoglobals
var countryMapping = make(map[string]string)
var _ = json.Unmarshal(resources.CountryToContinentJSON, &countryMapping)

type IPInfo struct {
	IP        string `json:"ip"`
	City      string `json:"city"`
	Region    string `json:"region"`
	Country   string `json:"country"`
	Continent string `json:"continent"`
	Loc       string `json:"loc"`
	Org       string `json:"org"`
	Postal    string `json:"postal"`
	Timezone  string `json:"timezone"`
	Bogon     bool   `json:"bogon"`
	Latitude  float32
	Longitude float32
	ASN       string
	ISP       string
}

func (i *IPInfo) Resolve() {
	loc := strings.Split(i.Loc, ",")
	//nolint:gomnd
	if len(loc) == 2 {
		if lat, err := strconv.ParseFloat(loc[0], 32); err == nil {
			i.Latitude = float32(lat)
		}
		if long, err := strconv.ParseFloat(loc[1], 32); err == nil {
			i.Longitude = float32(long)
		}
	}

	//nolint:gomnd
	org := strings.SplitN(i.Org, " ", 2)
	if len(org) == 2 {
		i.ASN = org[0]
		i.ISP = org[1]
	}
}

func GetPublicIPInfo(ctx context.Context, ip string, token string) (IPInfo, error) {
	logger := logging.Logger("location_resolver")
	url := "https://ipinfo.io/json"
	if ip != "" {
		url = "https://ipinfo.io/" + ip + "/json"
	}

	if token != "" {
		url = url + "?token=" + token
	}

	logger.Debugf("Getting IP info for %s", ip)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return IPInfo{}, errors.Wrap(err, "failed to create http request")
	}

	client := &http.Client{}
	resp, err := client.Do(request)
	if err != nil {
		return IPInfo{}, errors.Wrap(err, "failed to get IP info")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return IPInfo{}, errors.New("failed to get IP info: " + resp.Status)
	}

	var ipInfo IPInfo
	err = json.NewDecoder(resp.Body).Decode(&ipInfo)
	if err != nil {
		return IPInfo{}, errors.Wrap(err, "failed to decode IP info")
	}

	if ipInfo.Bogon {
		return IPInfo{}, requesterror.BogonIPError{IP: ip}
	}

	ipInfo.Resolve()

	if continent, ok := countryMapping[ipInfo.Country]; ok {
		ipInfo.Continent = continent
	} else {
		logger.Error("Unknown country: " + ipInfo.Country)
		return IPInfo{}, errors.New("unknown country: " + ipInfo.Country)
	}

	logger.Debugf("Got IP info for %s: %+v", ip, ipInfo)
	return ipInfo, nil
}

type LocationResolver struct {
	localCache  *ttlcache.Cache[string, IPInfo]
	ipInfoToken string
	remoteTTL   int
}

type LocationCachePayload struct {
	IP        string `json:"ip"`
	IPPayload []byte `json:"ipPayload"`
	RemoteTTL int    `json:"ttl"`
}

func NewLocationResolver(ipInfoToken string, localTTL time.Duration, remoteTTL int) LocationResolver {
	localCache := ttlcache.New[string, IPInfo](
		ttlcache.WithTTL[string, IPInfo](localTTL),
		ttlcache.WithDisableTouchOnHit[string, IPInfo]())

	return LocationResolver{
		localCache,
		ipInfoToken,
		remoteTTL,
	}
}

func (l LocationResolver) ResolveIP(ctx context.Context, ip net.IP) (IPInfo, error) {
	ipString := ip.String()

	if ipInfo := l.localCache.Get(ipString); ipInfo != nil && !ipInfo.IsExpired() {
		return ipInfo.Value(), nil
	}

	if os.Getenv("IP_INFO_CACHE_URL") != "" {
		response, _ := http.NewRequest(
			http.MethodGet,
			os.Getenv("IP_INFO_CACHE_URL")+"/getIpInfo?ip="+ipString,
			nil)
		var ipInfo IPInfo
		if response.Response.StatusCode == http.StatusOK && response.Body != nil {
			err := json.NewDecoder(response.Body).Decode(&ipInfo)
			if err != nil {
				return IPInfo{}, errors.Wrap(err, "failed to decode IP info")
			}
			return ipInfo, nil
		}
	}

	ipInfo, err := GetPublicIPInfo(ctx, ipString, l.ipInfoToken)
	if err != nil {
		return IPInfo{}, errors.Wrap(err, "failed to get IP info")
	}

	ipJSON, err := json.Marshal(ipInfo)
	if err != nil {
		return IPInfo{}, errors.Wrap(err, "failed to unmarshal IP info")
	}

	l.localCache.Set(ipString, ipInfo, ttlcache.DefaultTTL)

	if os.Getenv("IP_INFO_CACHE_URL") != "" {
		requestBody, err := json.Marshal(LocationCachePayload{ipString, ipJSON, l.remoteTTL})
		if err != nil {
			return IPInfo{}, errors.Wrap(err, "Could not serialize IPInfo")
		}
		_, _ = http.NewRequest(
			http.MethodPost,
			os.Getenv("IP_INFO_CACHE_URL"),
			bytes.NewReader(requestBody))
	}
	return ipInfo, nil
}

func (l LocationResolver) ResolveIPStr(ctx context.Context, ip string) (IPInfo, error) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return IPInfo{}, requesterror.InvalidIPError{IP: ip}
	}

	return l.ResolveIP(ctx, parsed)
}

func (l LocationResolver) ResolveMultiaddr(ctx context.Context, addr multiaddr.Multiaddr) (IPInfo, error) {
	host, isHostName, _, err := DecodeMultiaddr(addr)
	if err != nil {
		return IPInfo{}, errors.Wrap(err, "failed to decode multiaddr")
	}

	if isHostName {
		ips, err := net.LookupIP(host)
		if err != nil {
			return IPInfo{}, requesterror.HostLookupError{Host: host, Err: err}
		}

		host = ips[0].String()
	}

	return l.ResolveIPStr(ctx, host)
}

func (l LocationResolver) ResolveMultiaddrsBytes(ctx context.Context, bytesAddrs []abi.Multiaddrs) (IPInfo, error) {
	return l.ResolveMultiaddrs(ctx, convert.AbiToMultiaddrsSkippingError(bytesAddrs))
}

func (l LocationResolver) ResolveMultiaddrs(ctx context.Context, addrs []multiaddr.Multiaddr) (IPInfo, error) {
	var lastErr error
	logger := logging.Logger("location_resolver")
	for _, addr := range addrs {
		ipInfo, err := l.ResolveMultiaddr(ctx, addr)
		if err != nil {
			lastErr = err
			logger.With("err", err).Debugf("Failed to resolve multiaddr %s", addr)
			continue
		}

		return ipInfo, nil
	}

	if lastErr != nil {
		return IPInfo{}, lastErr
	}

	return IPInfo{}, requesterror.NoValidMultiAddrError{}
}

type IsHostName = bool
type PortNumber = int
type IPOrHost = string

func DecodeMultiaddr(addr multiaddr.Multiaddr) (IPOrHost, IsHostName, PortNumber, error) {
	protocols := addr.Protocols()
	isHostName := false
	const expectedProtocolCount = 2

	if len(protocols) != expectedProtocolCount {
		return "", false, 0, errors.New("multiaddr does not contain two protocols")
	}

	if !slices.Contains(
		[]int{
			multiaddr.P_IP4, multiaddr.P_IP6,
			multiaddr.P_DNS4, multiaddr.P_DNS6,
			multiaddr.P_DNS, multiaddr.P_DNSADDR,
		}, protocols[0].Code,
	) {
		return "", false, 0, errors.New("multiaddr does not contain a valid ip or dns protocol")
	}

	if slices.Contains(
		[]int{
			multiaddr.P_DNS, multiaddr.P_DNSADDR,
			multiaddr.P_DNS4, multiaddr.P_DNS6,
		}, protocols[0].Code,
	) {
		isHostName = true
	}

	if protocols[1].Code != multiaddr.P_TCP {
		return "", false, 0, errors.New("multiaddr does not contain a valid tcp protocol")
	}

	splitted := multiaddr.Split(addr)

	component0, ok := splitted[0].(*multiaddr.Component)
	if !ok {
		return "", false, 0, errors.New("failed to cast component")
	}

	host := component0.Value()

	component1, ok := splitted[1].(*multiaddr.Component)
	if !ok {
		return "", false, 0, errors.New("failed to cast component")
	}

	port, err := strconv.Atoi(component1.Value())
	if err != nil {
		return "", false, 0, errors.Wrap(err, "failed to parse port")
	}

	return host, isHostName, port, nil
}
