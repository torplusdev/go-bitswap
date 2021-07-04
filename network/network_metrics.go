package network

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	bspaym "paidpiper.com/payment-gateway/paymentmanager"
)

type MetricsStore struct {
	registry *prometheus.Registry
	source   bspaym.MetricsSource
}

func NewMetricsStore() *MetricsStore {
	s := &MetricsStore{}
	s.createRegestry()

	return s
}

// Size of toFetch queue [BitswapFetchQueueSize]
// liveWants map taken capacity [BitswapLiveWantsCount]
// age [seconds] of the oldest request [BitswapLiveWantsOldestAge]
// age [seconds] of the first request (the one at the head of liveWantsOrder) [BitswapLiveWantsFirstAge]
func (s *MetricsStore) createRegestry() {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Name: "bitswap_size_of_fetch_queue",
				Help: "Size of toFetch queue [BitswapFetchQueueSize]",
			},
			func() float64 {
				return float64(s.source.FetchQueueCount())
			}),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Name: "bitswap_live_wants",
				Help: "liveWants map taken capacity [BitswapLiveWantsCount]",
			},
			func() float64 {
				return float64(s.source.LiveWantsCount())
			}),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Name: "bitswap_oldest_request_age",
				Help: "age [seconds] of the oldest request [BitswapLiveWantsOldestAge]",
			},
			func() float64 {
				return float64(s.source.LiveWantsOldestAge())
			}),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Name: "bitswap_first_request_age",
				Help: "age [seconds] of the first request (the one at the head of liveWantsOrder) [BitswapLiveWantsFirstAge]",
			},
			func() float64 {
				return float64(s.source.LiveWantsFirstAge())
			}),
	)

	s.registry = registry
}

func (s *MetricsStore) SetSource(source bspaym.MetricsSource) {
	s.source = source
}
func (s *MetricsStore) Handler() http.Handler {
	return promhttp.InstrumentMetricHandler(
		s.registry, promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}),
	)
}
