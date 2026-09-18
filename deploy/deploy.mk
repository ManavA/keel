# Makefile fragment for a keel-based service's deploy targets. Include it
# from the project's own Makefile:
#
#   include deploy/deploy.mk
#
# Required variables (set them in the including Makefile, or on the command
# line): SERVICE_NAME, GCP_PROJECT.
#
# deploy/cloudbuild.yaml's _REGION and _REPOSITORY substitutions default to
# us-central1 and services; override either by adding
# --substitution _REGION=... or --substitution _REPOSITORY=... below.

.PHONY: deploy-image
deploy-image:
	@test -n "$(SERVICE_NAME)" || (echo "SERVICE_NAME is not set" >&2; exit 1)
	@test -n "$(GCP_PROJECT)" || (echo "GCP_PROJECT is not set" >&2; exit 1)
	scripts/deploy-cloudbuild.sh deploy/cloudbuild.yaml \
		--project "$(GCP_PROJECT)" \
		--substitution _SERVICE_NAME="$(SERVICE_NAME)"

.PHONY: verify-deploy
verify-deploy:
	@test -n "$(SERVICE_URL)" || (echo "SERVICE_URL is not set" >&2; exit 1)
	scripts/verify-deployed.sh "$(SERVICE_URL)"

.PHONY: compose-up
compose-up:
	docker compose -f deploy/compose.yaml up

.PHONY: compose-down
compose-down:
	docker compose -f deploy/compose.yaml down
