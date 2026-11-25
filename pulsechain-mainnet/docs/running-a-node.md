# Running a PulseChain Node

This guide covers running your own PulseChain mainnet node, from the easiest Docker Compose setup to advanced and expert options.

## Start with an AI agent (optional)

Prefer not to run the steps yourself? Paste this into an AI agent with terminal and Docker access. Same hardware/Docker needs as the Compose section below.

```text
Help me run a local PulseChain full node.

Get the project onto this machine if needed: prefer `git clone https://gitlab.com/pulsechaincom/pulsechain-mainnet.git` when Git is available;
otherwise download and unzip https://gitlab.com/pulsechaincom/pulsechain-mainnet/-/archive/master/pulsechain-mainnet-master.zip.
Open the project and follow the agent skills in `.agents/skills/` — start with `setup-node`.
Prefer those skills and docs/running-a-node.md over guessing. Ask before destructive steps.
```

After setup, ask in plain language as needed (for example “is my node synced?” or “upgrade the clients”) — `sync-status` and `upgrade-clients` cover those.

## Run a Local RPC Node (Easiest)

These steps start a local PulseChain full node and expose JSON-RPC at `http://127.0.0.1:8545` for MetaMask and other apps. Prefer this path unless you need a custom client setup (see [Advanced Users](#advanced-users-running-a-pulsechain-node) below).

You will need a computer with roughly **4+ CPU cores**, **16 GB+ RAM**, and **2 TB+ free SSD** space. The first sync can take a long time, so leave the machine on with Docker running.

**1. Install Docker Desktop**

Install [Docker](https://docs.docker.com/get-docker/) (Docker Compose is included with Docker Desktop).

**2. Check disk space**

Make sure you have at least **2 TB free** on the drive where this project folder lives.

**3. Prepare the data folder**

Open a terminal in this project folder (the one that contains `docker-compose.yml`), then run the setup script. It creates a `data` folder and a shared secret file the two node containers use to talk to each other:

- Mac / Linux:
  ```shell
  ./scripts/setup.sh
  ```
- Windows (Command Prompt or double-click):
  ```bat
  .\scripts\setup.bat
  ```

**4. Start the node**

```shell
docker compose up -d
```

The RPC endpoint will appear at the address below, but **it is not ready for normal wallet or app use until the node finishes syncing**. The first sync can take days (or longer) depending on your hardware and network, so leave the machine on with Docker running. Use the sync check below; when it returns `false`, you can point MetaMask and other apps at:


| Setting         | Value                         |
| --------------- | ----------------------------- |
| RPC URL         | `http://127.0.0.1:8545`       |
| Chain ID        | `369`                         |
| Currency Symbol | `PLS`                         |
| Block explorer  | `https://scan.pulsechain.com` |


**Useful commands**

- Watch progress: `docker compose logs -f`
- Check if still syncing:
  - Mac / Linux:
    ```shell
    curl -s -X POST http://127.0.0.1:8545 -H 'content-type: application/json' --data '{"jsonrpc":"2.0","method":"eth_syncing","params":[],"id":1}'
    ```
  - Windows (PowerShell):
    ```powershell
    Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8545 -ContentType 'application/json' -Body '{"jsonrpc":"2.0","method":"eth_syncing","params":[],"id":1}' | Format-List
    ```
  When sync is finished, the result is `false` (or `False` in PowerShell).
- Stop the node: `docker compose down`



## Advanced Users: Running a PulseChain Node

> Prefer the [Docker Compose section above](#run-a-local-rpc-node-easiest) unless you need custom clients or paths.

If you have an archive node running on Ethereum Mainnet or PulseChain Testnet-V4, you can save some sync time by rolling back and re-using your existing blockchain DB. See [Using An Existing Blockchain DB](#experts-only-using-an-existing-blockchain-db) below.

> **Warning**: PulseChain includes **all** of the Ethereum mainnet state up to block `17,232,999`. This means that the system requirements for running a node will be high, particularly the storage requirements.

HARDWARE

- A fast CPU with 4+ cores
- 16 GB+ of RAM
- 1.5 TB+ free storage (SSD) for a Full Node
- 7 TB+ free storage (SSD) for an Archive Node

SOFTWARE

- [Docker](https://docs.docker.com/get-docker/) is recommended, and the commands below will tailored to running a dockerized node. By building and running the node in docker, we eliminate any environmental differences like the local golang version or the host OS.
- If you prefer, you can compile and run the executable directly, but you will need to tweak the commands below.

> **NOTE**: All commands below assume that you want to store all chain data in a local `/blockchain` directory (must have at least **1.5 TB** free space).
>
> If needed, you can modify the commands to mount a different directory in the docker container. To do so, you will change the **absolute path** on the **left side** of the colon `:`, e.g., `docker run -v /path/to/my/dir/:/blockchain ...`
>
> For more information see the Docker [run command reference](https://docs.docker.com/engine/reference/commandline/run/#mount-volume--v---read-only).



### 1. Prepare the Blockchain Directory

First, ensure that the intended blockchain datadir has at least 1.5 TB of free space. The directory should be empty.

### 2. Generate JWT Secret

The HTTP connection between your beacon node and execution node needs to be authenticated using a [JWT token](https://jwt.io/). We need to save this file in the blockchain directory, to be read by both clients. There are several ways to generate this JWT token:

- Use a utility like OpenSSL to create the token via command: `openssl rand -hex 32 | tr -d "\n" > "/blockchain/jwt.hex"`.
- Use an online generator like [this](https://seanwasere.com/generate-random-hex/). Copy and paste this value into a `/blockchain/jwt.hex` file.



### 3. Start the PulseChain Execution Client

Once your blockchain directory is ready, you can start the execution client and connect to the network by providing the `pulsechain` flag. You can run either Go-Pulse (recommended) **OR** Erigon-Pulse:

Option 1: [Go-Pulse](https://gitlab.com/pulsechaincom/go-pulse)

```shell
docker run --network=host -v /blockchain:/blockchain registry.gitlab.com/pulsechaincom/go-pulse \
--pulsechain \
--authrpc.jwtsecret=/blockchain/jwt.hex \
--datadir=/blockchain/execution
```

Option 2: [Erigon-Pulse](https://gitlab.com/pulsechaincom/erigon-pulse)

```shell
docker run --network=host -v /blockchain:/blockchain registry.gitlab.com/pulsechaincom/erigon-pulse \
--chain=pulsechain \
--authrpc.jwtsecret=/blockchain/jwt.hex \
--datadir=/blockchain/execution \
```

> If using Erigon-Pulse older than `v2.3.0`, you need to include the `--externalcl` flag to disable the experimental internal CL. Starting with `v2.3.0`, this is off by default.



### 4. Start the PulseChain Consensus Client

Once your execution client is running, you can start the consensus client and connect to the network by providing the `pulsechain` flag. You can run either Prysm-Pulse (recommended) **OR** Lighthouse-Pulse:

Option 1: [Prysm-Pulse](https://gitlab.com/pulsechaincom/prysm-pulse)

```shell
docker run --network=host -v /blockchain:/blockchain registry.gitlab.com/pulsechaincom/prysm-pulse/beacon-chain \
--pulsechain \
--jwt-secret=/blockchain/jwt.hex \
--datadir=/blockchain/consensus \
--checkpoint-sync-url=https://checkpoint.pulsechain.com \
--genesis-beacon-api-url=https://checkpoint.pulsechain.com
```

Option 2: [Lighthouse-Pulse](https://gitlab.com/pulsechaincom/lighthouse-pulse)

```shell
docker run --network=host -v /blockchain:/blockchain registry.gitlab.com/pulsechaincom/lighthouse-pulse \
--network=pulsechain \
--execution-jwt=/blockchain/jwt.hex \
--datadir=/blockchain/consensus \
--execution-endpoint=http://localhost:8551 \
--checkpoint-sync-url https://checkpoint.pulsechain.com \
--http
```



## Experts Only: Using An Existing Blockchain DB

> **Warning**: This is only valid for **archive nodes** that were sync'd with a previous version of the **PulseChain Testnet** or the **Ethereum Mainnet**.

> We will only reuse an existing execution-layer database since PulseChain starts with a new Beacon chain.



### 1. Stop the Existing Blockchain

Stop any existing Go-Pulse or Go-Ethereum processes and let the blockchain gracefully shut down.

### 2. Dump New Genesis File

Dump the `genesis.json` file from the latest Go-Pulse release. It is recommended you dump this file into your existing `--datadir` used by the previously running node. Assuming the `/blockchain` directory was being used, we can dump the updated `genesis.json` file with the command below.

```shell
docker run registry.gitlab.com/pulsechaincom/go-pulse --pulsechain dumpgenesis > /blockchain/genesis.json
```

Confirm that the `genesis.json` file has been written to your blockchain datadir. Double check the modified date to ensure this file was just created/updated.

### 3. Perform Rollback

We will need to rollback to **the last ethereum mainnet block in your DB less than `17233000`.

- If running an Ethereum Mainnet node, you can rollback to `17232999` (`0x106F467`)
- If running a PulseChain Testnet-V4 node, you will need to rollback further to `16492699` (`0xFBA89B`)

In order to rollback the chain, we need to launch the geth console to issue the `debug.setHead()` command. It's important to run with the `--nodiscover` flag to prevent the node from syncing any new blocks during this process.

```shell
docker run -it registry.gitlab.com/pulsechaincom/go-pulse --pulsechain --nodiscover console
```

**From the interactive console, verify chain state and perform the rollback:**

1. Perform the chain rollback to block the block number shown above:
  ```shell
    # for eth mainnet archive
    > debug.setHead("0x106F467")

    # for pulsechain-testnet-v4 archive
    > debug.setHead("0xFBA89B")
  ```
2. Verify the block number has been updated
  ```shell
    > eth.blockNumber
    17232999
  ```
3. Exit the geth console
  ```shell
    > exit
  ```



### 4. Re-Initialize Genesis w/ Updated Chain Config

With the blockchain rolled back to the last ethereum mainnet block, you can now reinitialize the genesis for PulseChain, using the `genesis.json` file you dumped in step 2.

```shell
docker run -v /blockchain:/blockchain registry.gitlab.com/pulsechaincom/go-pulse --datadir=/blockchain init /blockchain/genesis.json
```

After the init command has completed, you can follow the normal steps above: [Advanced Users: Running a PulseChain Node](#advanced-users-running-a-pulsechain-node).

- You will use the existing execution directory for your execution node only.
- It's recommended to move the execution db data into a subdirectory such as `/blockchain/execution`.