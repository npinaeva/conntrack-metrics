#!/bin/bash

mkdir -p results
kubectl wait --for jsonpath='{.status.phase}'=Succeeded pod -l app=client -n workload-client-0 --timeout=5m
kubectl logs -l app=client -n workload-client-0 --tail=-1 --prefix=true > ./results/kube-burner-logs
cat ./results/kube-burner-logs
