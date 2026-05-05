# Librescoot Keycard Service

NFC keycard authentication service for Librescoot vehicles using the PN7150 NFC controller.

Part of the [Librescoot](https://librescoot.org/) open-source platform.

## Features

- **NFC Tag Detection**: Reads NFC/RFID tags via PN7150 controller
- **Master Card Learning**: Initial setup allows designating a master card
- **Authorization Management**: Master card can authorize additional cards
- **Redis Integration**: Publishes authentication events via Redis
- **RGB LED Feedback**: Visual feedback using LP5562 LED controller or shell scripts
- **Flexible LED Backend**: Supports both direct I2C control (LP5562) and script-based control

## Hardware Requirements

- PN7150 NFC controller on `/dev/pn5xx_i2c2`
- Optional: LP5562 RGB LED controller on I2C bus (default: `/dev/i2c-2` at address `0x30`)

## Installation

```bash
# Build for ARM (Raspberry Pi, etc.)
make build

# Build for native architecture
make build-native
```

The binary will be created at `bin/keycard-service`.

## Usage

```bash
# Basic usage with script-based LED control
./keycard-service

# With LP5562 RGB LED support
./keycard-service --led-device /dev/i2c-2 --led-address 0x30

# Custom configuration
./keycard-service \
  --device /dev/pn5xx_i2c2 \
  --data-dir /data/keycard \
  --redis localhost:6379 \
  --log 3
```

### Command Line Options

- `--device`: NFC device path (default: `/dev/pn5xx_i2c2`)
- `--data-dir`: Directory for storing UID files (default: `/data/keycard`)
- `--redis`: Redis server address (default: `localhost:6379`)
- `--log`: Log level 0-3 (0=error, 1=warn, 2=info, 3=debug, default: 2)
- `--led-device`: I2C device for LP5562 LED (empty for script-based control)
- `--led-address`: I2C address for LP5562 LED (default: `0x30`)

## Operation

### Initial Setup

1. Start the service without a master card configured
2. Service enters master learning mode (LED blinks)
3. Present the master card to register it
4. LED flashes to confirm registration

### Normal Operation

- **Authorized Card**: Green LED flash, authentication published to Redis
- **Unauthorized Card**: Red LED flash
- **Master Card**: Enters learning mode (LEDs 3 and 7 turn on)

### Learning Mode

1. Present master card to enter learning mode
2. Present cards to authorize (LED flashes green for each)
3. Present master card again to exit learning mode

## LED Feedback

### LP5562 RGB LED (Hardware)
- **Green**: Authorized card
- **Red**: Unauthorized card
- **Amber**: Tag lookup in progress
- **Blinking**: Master learning mode

### Script-based LED Control
If `--led-device` is not specified, the service calls:
- `/usr/bin/greenled.sh` for RGB control
- `/usr/bin/ledcontrol.sh` for pattern control

## Data Storage

UID files are stored in the data directory (default: `/data/keycard/`):
- `master_uids.txt`: Master card UIDs (one per line)
- `authorized_uids.txt`: Authorized card UIDs (one per line)

## Redis Events

When an authorized card is presented, the service publishes to Redis:

```
HSET keycard authentication "passed"
HSET keycard type "scooter"
HSET keycard uid "<card-uid>"
PUBLISH keycard "authentication"
EXPIRE keycard 10
```

The hash expires after 10 seconds.

## Development

### Dependencies

- Go 1.24.1 or later
- [github.com/librescoot/pn7150](https://github.com/librescoot/pn7150) - PN7150 NFC library
- [github.com/redis/go-redis/v9](https://github.com/redis/go-redis) - Redis client

### Building

```bash
# ARM build (for embedded Linux)
make build

# Native build (for development)
make build-native

# Clean build artifacts
make clean
```

## License

This project is dual-licensed. The source code is available under the
[Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License][cc-by-nc-sa].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png

## Part of Librescoot

This service is part of the [Librescoot](https://github.com/librescoot) project, an open-source vehicle control system.
