package jmap

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// What resolving a JMAP id costs: an id names a message by GUID with no folder
// in it, so before the per-user store this was a walk (#1711).
var (
	metricIDLookup = promauto.NewCounter(prometheus.CounterOpts{
		Name: "jmap_id_lookup_total",
		Help: "Requests that resolved one or more JMAP ids to messages.",
	})
	metricIDLookupFolders = promauto.NewCounter(prometheus.CounterOpts{
		Name: "jmap_id_lookup_folders_opened_total",
		Help: "Folders opened while resolving JMAP ids.",
	})
	metricIDLookupRecords = promauto.NewCounter(prometheus.CounterOpts{
		Name: "jmap_id_lookup_records_read_total",
		Help: "Index records read while resolving JMAP ids.",
	})
	metricIDLookupSource = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "jmap_id_lookup_source_total",
		Help: "Where a lookup was answered from: store is the per-user GUID store, walk is the folder scan it replaces.",
	}, []string{"source"})
)
