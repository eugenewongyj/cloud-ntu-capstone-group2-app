# Upstream reference material (not used by our pipeline)

These files come from the upstream AWS Containers Retail Store Sample
(https://github.com/aws-containers/retail-store-sample-app).

They were moved here from `.github/workflows/`, `.github/actions/` and `terraform/`
so they no longer run in our repository. GitHub Actions only runs workflows in
`.github/workflows/`.

Per the capstone brief, upstream deployment and infrastructure examples are
reference material only. Our own implementation lives in:

- `.github/workflows/` — our CI security gates and build/deploy workflows
- `k8s/` — our Kubernetes manifests
- the infra repo `cloud-ntu-capstone-group2-infra` — our Terraform and GitOps (Argo CD)