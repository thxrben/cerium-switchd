module github.com/thxrben/cerium-switchd/lib/bgp

go 1.27.1

require (
	github.com/osrg/gobgp/v3 v3.37.0
	github.com/thxrben/cerium-switchd/lib/conf v0.0.0-00010101000000-000000000000
)

require github.com/stretchr/testify v1.11.1 // indirect

replace github.com/thxrben/cerium-switchd/lib/conf => ../conf
