FROM alpine:edge
  
COPY kube-to-coredns /kube-to-coredns
COPY corefile /corefile
RUN mkdir -p /zones

ENTRYPOINT ["/kube-to-coredns"]
