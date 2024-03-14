## Workload

This workload is used to test connection latency. It uses apache benchmark as workload generator.

We want to test kube-proxy performance on different service workloads, at the same time we want to minimize the 
impact of client/server performance. To achieve that, we will use the exact same amount of service backends and
the same configuration for clients.
The main tests parameters are (set in the [env](./env) file):
- number of services (SERVICES, using the same amount of backends)
- number of clients (CLIENTS, when number of services is more than number of clients, each client will ping its own service
by picking equally distributed service ips, e.g. for 10 clients and 100 services, 
client-1 pings service-10, client-2 pings service-20, etc.)
- number of requests per client (CONN_PER_CLIENT, `ab -n` value)
- client concurrency (CLIENT_CONCURRENCY, number of parallel connections per client, `ab -c` value)

The total number of requests = `CONN_PER_CLIENT * CLIENTS`

## Running

1. Install `kube-burner` v1.9.0+

   1.1 You can download `kube-burner` from https://github.com/cloud-bulldozer/kube-burner/releases

   1.2 You can build it from source [kube-burner](https://github.com/cloud-bulldozer/kube-burner/tree/main) with
   `make build`
2. Set test parameters in the `env` file, make sure to set KUBECONFIG either in the `env` file or in
the terminal itself.
3. `source ./env && kube-burner init -c ./kube-burner.yaml`

kube-burner cleans up automatically, but if you interrupted the run, use `kubectl delete ns -l kube-burner-job` to clean up.

## Results

Check grafana dashboard for the results. `ab` results are stored in the `./results` folder (created during the test run).
You will find connect and total time distributions reported by the client pods in `results/connect_time.jpg` and `results/total_time.jpg`.
`results/quantiles` shows quantiles calculated for the reported measurements from ab.

**NOTE**: times reported by `ab` are always higher than results reported by the conntrack-metrics and the dashboard
as it includes `ab`'s send and receive delay.

The easiest way to have fresh results for every experiment is to just restart the conntrack-metrics pods. 