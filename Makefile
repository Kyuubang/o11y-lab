DOCKER ?= docker
COMPOSE = $(DOCKER) compose

.PHONY: up down nuke ps logs build smoke load stress spike

up: ## build and start the whole lab
	$(COMPOSE) up -d --build

down: ## stop the lab, keep data
	$(COMPOSE) down

nuke: ## stop the lab and delete all data volumes
	$(COMPOSE) down -v

ps:
	$(COMPOSE) ps

logs:
	$(COMPOSE) logs -f --tail=100

build:
	$(COMPOSE) build

smoke: ## functional + telemetry smoke test
	./scripts/smoke.sh

load: ## steady k6 load (DURATION=3m)
	DOCKER=$(DOCKER) ./scripts/stress.sh load

stress: ## ramping k6 stress (~6m)
	DOCKER=$(DOCKER) ./scripts/stress.sh stress

spike: ## k6 spike (~2m)
	DOCKER=$(DOCKER) ./scripts/stress.sh spike
