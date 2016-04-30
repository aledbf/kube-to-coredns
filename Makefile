all: push

# 0.0 shouldn't clobber any release builds
TAG = 0.0
PREFIX = aledbf/kube-to-coredns

controller: clean
	CGO_ENABLED=0 GOOS=linux godep go build -a -ldflags '-w' -o kube-to-coredns
	goupx kube-to-coredns

container: controller
	docker build -t $(PREFIX):$(TAG) .

push: container
	docker push $(PREFIX):$(TAG)

clean:
	rm -f kube-to-coredns
