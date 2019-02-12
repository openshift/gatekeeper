#!/bin/bash

cd "${0%/*}"

echo "Deploy Kubernetes policies"

read -p "Press enter to continue"

# deploy kubernetes policies
kubectl -n gatekeeper-system create configmap kubernetes-matches --from-file=../policy/kubernetes/matches.rego
kubectl -n gatekeeper-system create configmap kubernetes-policymatches --from-file=../policy/kubernetes/policymatches.rego
