module keycard-service

go 1.22.1

require (
	github.com/librescoot/pn7150 v0.1.3
	github.com/librescoot/redis-ipc v0.10.1
	golang.org/x/sys v0.30.0
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/redis/go-redis/v9 v9.7.3 // indirect
)

replace github.com/librescoot/pn7150 => /home/teal/src/librescoot/pn7150
