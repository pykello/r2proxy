BIN := dist/r2proxy

.PHONY: build run static docker deploy push destroy fmt vet clean

build: ## build local binary
	go build -ldflags="-s -w" -o $(BIN) .

static: ## build static linux/amd64 binary
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BIN) .

run: build ## build + run (needs R2PROXY_ENDPOINT/ACCESS_KEY/SECRET_KEY env)
	./$(BIN) serve

docker: ## build docker image
	docker build -t r2proxy:latest .

deploy: ## provision Ubicloud VM and deploy (see deploy.env)
	./deploy.sh up

push: ## rebuild + redeploy to existing VM
	./deploy.sh push

destroy: ## tear down the Ubicloud VM
	./deploy.sh destroy

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -rf dist r2proxy.json r2proxy.json.tmp *.log
