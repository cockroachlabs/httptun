.PHONY: local
local:
	docker build -t localhost:5000/httptun:latest .

.PHONY: push/local
push/local: local
	docker push localhost:5000/httptun:latest
