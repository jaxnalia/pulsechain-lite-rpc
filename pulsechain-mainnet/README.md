# PulseChain Mainnet

The PulseChain Mainnet is up and running at last! This document will guide you through connecting Metamask to the network.

## Exploring PulseChain

Public block and beacon explorers are available at the URLs below:

- [PulseChain Block Explorer](https://scan.pulsechain.com)
- [PulseChain Beacon Explorer](https://beacon.pulsechain.com)

> The PulseChain team welcomes the development of 3rd-party explorers!

## Connecting Metamask

Follow these instructions to manually add the PulseChain to your Metamask plugin. A button will be available in the future to do this automatically.

**1. Click the Networks dropdown and select `Add network`** ([Screenshot](images/step1.png)).

**2. At the bottom of the network list, click `Add a network manually`** ([Screenshot](images/step2.png)).

**3. Enter the following information into the form and `save`:** ([Screenshot](images/step3.png)).
- Network Name: `PulseChain`
- New RPC URL: `https://rpc.pulsechain.com`
- Chain ID: `369`
- Currency Symbol: `PLS`
- Block explorer URL: `https://scan.pulsechain.com`

**Congratulations**! You are now connected to PulseChain! Existing ethereum accounts that had balances as of block `17,232,999` (*May-10-2023 10:36:11 PM +UTC*) will have the equivalent balance on PulseChain in addition to any credits from the sacrifice phase as well as ethereum staking deposit refunds.

## Running Your Own Node

Want a local RPC or to operate your own full/archive node? See **[Running a PulseChain Node](docs/running-a-node.md)** for Docker Compose (easiest) and advanced setup instructions.

## Staking and Running a Validator

**Running a Validator is a responsibility to keep your node healthy and running, or your deposited funds will be at risk.** A `32 million tPLS` deposit will be required to register a validator.

If you are interested in running a Validator for the PulseChain head over to our [Staking Launchpad](https://launchpad.pulsechain.com).
