# Culprit demo stack. Run from the repo root with `make -C demo <target>`
# or from this folder with `make <target>`.
COMPOSE := docker compose -f docker-compose.yml
SCENARIO ?=

.PHONY: up down logs ps chaos reset build grafana clean

up: ## build and start the stack, wait until healthy
	@mkdir -p changes && touch changes/changes.jsonl
	$(COMPOSE) up -d --build --wait
	@printf 'Waiting for Loki'; for i in $$(seq 1 60); do \
	  curl -fsS http://127.0.0.1:3100/ready >/dev/null 2>&1 && break; printf '.'; sleep 2; done; echo
	@echo "Stack is up. Frontend http://127.0.0.1:8080  Prometheus http://127.0.0.1:9090  Loki http://127.0.0.1:3100"
	@echo "Let it run ~10 minutes for a baseline, then: make chaos SCENARIO=bad-deploy"

grafana: ## start the stack plus Grafana on http://127.0.0.1:3000
	$(COMPOSE) --profile grafana up -d --build --wait

down: ## stop the stack and delete its data volumes
	$(COMPOSE) --profile grafana down -v

logs: ## follow app logs
	$(COMPOSE) logs -f --tail=50 frontend catalog cart

ps:
	$(COMPOSE) ps

build:
	$(COMPOSE) build

chaos: ## inject a scenario: make chaos SCENARIO=bad-deploy|pool-leak|redis-down|bad-config
	@test -n "$(SCENARIO)" || { echo "usage: make chaos SCENARIO=bad-deploy|pool-leak|redis-down|bad-config"; exit 2; }
	./chaos $(SCENARIO)

reset: ## restore the healthy stack
	./chaos reset

clean: down ## stop everything and forget recorded changes
	rm -f changes/changes.jsonl
