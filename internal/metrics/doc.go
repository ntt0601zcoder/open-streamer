// Package metrics writes the Prometheus text exposition format (version
// 0.0.4) without any client library: a Registry runs its collectors at every
// scrape and a Writer checks and renders what they produce.
package metrics
