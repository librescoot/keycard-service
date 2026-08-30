# Librescoot Keycard Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

`keycard-service` provides NFC keycard authentication for Librescoot vehicles.
It reads tags through a PN7150 controller, stores local authorization state,
and publishes authentication and administration events through Redis.

## Capabilities

- Detects NFC tags through a PN7150 device.
- Learns and manages master and authorized card UIDs.
- Publishes successful authentication events for vehicle consumers.
- Supports Redis command-driven card administration and learn modes.
- Provides LED feedback through an LP5562 controller or the installed LED
  control scripts.

## Operation and Redis interface

On first start without a master UID, the service enters master-learning mode.
A master-card tap enters regular learn mode; cards collected during that mode
are saved when the master card is tapped again. Authorized-card taps publish a
transient `keycard` hash with `authentication=passed`, `type=scooter`, and the
UID, then set a 10-second expiration. The notification is published on the
`keycard` channel with payload `authentication`.

Master and authorized counts are published in the `system` hash as
`keycard-master-count` and `keycard-authorized-count`.

Administrative commands are read from the `scooter:keycard` Redis list. They
cover listing, counting, adding, and removing authorized UIDs; setting a
master; regular and master teach-in; and reset. Results are written to
`keycard.command-result`. Learn-mode events are published on `keycard:events`.
Use the source-defined command vocabulary and result format when integrating;
this README intentionally does not duplicate generated or protocol-level help.

## Configuration and local data

Run `bin/keycard-service -help` after building for the authoritative flag list.
The relevant deployment settings select the PN7150 device, Redis address, UID
data directory, logging, and optional LP5562 I2C device/address.

By default, UIDs are stored under `/data/keycard` in `master_uids.txt` and
`authorized_uids.txt`. When no LP5562 device is configured, LED feedback uses
`/usr/bin/greenled.sh` and `/usr/bin/ledcontrol.sh`.

UID files are authorization data, and the Redis command list can change them.
Protect both from untrusted local users and services. NFC UID matching alone is
not a general-purpose credential-security guarantee.

## Build and test

```bash
make build        # Linux ARMv7 binary: bin/keycard-service
make build-host   # local-development binary: bin/keycard-service
make test
make lint         # requires golangci-lint
```

`make run` starts the service through `go run`; it still needs its configured
hardware and Redis dependencies.

## Deployment and operations

The Yocto layer ships `librescoot-keycard.service`, which requires Valkey,
starts after the vehicle service, and enables the LP5562 backend on
`/dev/i2c-2`. The runtime requires a reachable Redis-compatible datastore,
access to the configured PN7150 device, and a writable data directory. LP5562
support additionally requires access to the configured I2C device; otherwise
the two LED scripts must be present if visual feedback is required.

The service handles `SIGINT` and `SIGTERM` and closes its NFC, LED, and Redis
resources during shutdown.

## License

This project is licensed under the [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](LICENSE).

Made with ❤️ by the Librescoot community
