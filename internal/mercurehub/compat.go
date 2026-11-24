//go:build !nomercure && deprecated_topic && deprecated_claim

package mercurehub

// compatible says that the hub was built with what it needs to take the
// clients and tokens of the protocol before 1.0 (its tags deprecated_topic
// and deprecated_claim), as the Makefile and the static build do.
const compatible = true
