#!/bin/sh

CONFIG_FILE=$HOME/.lightning-jet/config.json
ENV_FILE=docker/.env

mkdir -p $HOME/.lightning-jet

[ -f "$CONFIG_FILE" ] || cat << 'CONFIG' > $CONFIG_FILE 
{
  "avoid": [],
  "macaroonPath": "/home/umbrel/umbrel/lnd/data/chain/bitcoin/mainnet/admin.macaroon",
  "tlsCertPath": "/home/umbrel/umbrel/lnd/tls.cert",
  "rebalancer": {
    "maxTime": 30,
    "maxPpm": 650,
    "maxAutoPpm": 500,
    "maxInstances": 10,
    "maxPendingHtlcs": 4,
    "enforceMaxPpm": false,
    "exclude": []
  }
}
CONFIG

[ -f "$ENV_FILE" ] || cat << 'ENV' > $ENV_FILE 
COMPOSE_PROJECT_NAME=lightning-jet
LND_DIR=/home/umbrel/umbrel/app-data/lightning/data/lnd
LND_CONFIG_FILE=/home/umbrel/.lightning-jet/config.json
LND_HOSTNAME=umbrel.local
LND_IP_ADDRESS=10.21.21.9
ENV
