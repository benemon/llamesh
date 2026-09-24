# The page is built by Vite into web/dist and copied into the Go embed directory before the build.
.PHONY: build web test
build: web
	rm -rf internal/web/dist && cp -R web/dist internal/web/dist && touch internal/web/dist/.gitkeep
	go build -o llamesh ./cmd/llamesh
web:
	cd web && npm run build
test:
	go vet ./... && go test ./...
