# etcd

`etcd` enabled reading zone data from an etcd instance. The data in etcd has to be encoded as
a [message](https://github.com/skynetservices/skydns/blob/2fcff74cdc9f9a7dd64189a447ef27ac354b725f/msg/service.go#L26)
like [SkyDNS](https//github.com/skynetservices/skydns). It should also work just like SkyDNS.

The etcd middleware makes extensive use of the proxy middleware to forward and query other servers
in the network.

## Syntax

~~~
etcd [zones...]
~~~

* `zones` zones etcd should be authoritative for.

The path will default to `/skydns` the local etcd proxy (http://localhost:2379).
If no zones are specified the block's zone will be used as the zone.

If you want to `round robin` A and AAAA responses look at the `loadbalance` middleware.

~~~
etcd [zones...] {
    stubzones
    path /skydns
    endpoint endpoint...
    upstream address...
    tls cert key cacert
}
~~~

* `stubzones` enable the stub zones feature. The stubzone is *only* done in the etcd tree located
    under the *first* zone specified.
* `path` the path inside etcd, defaults to "/skydns".
* `endpoint` the etcd endpoints, default to "http://localhost:2397".
* `upstream` upstream resolvers to be used resolve external names found in etcd, think CNAMEs
  pointing to external names. If you want CoreDNS to act as a proxy for clients you'll need to add
  the proxy middleware.
* `tls` followed the cert, key and the CA's cert filenames.

## Examples

This is the default SkyDNS setup, with everying specified in full:

~~~
.:53 {
    etcd skydns.local {
        stubzones
        path /skydns
        endpoint http://localhost:2379
        upstream 8.8.8.8:53 8.8.4.4:53
    }
    prometheus
    cache 160 skydns.local
    loadbalance
    proxy . 8.8.8.8:53 8.8.4.4:53
}
~~~
