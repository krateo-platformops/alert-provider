# hyperdx-proof

Metric and ratio Alerts against a **real HyperDX 2.35.0** (ClickStack 3.2.0, the chart
krateo-observability wraps) on a disposable kind cluster. alert-provider runs from this source tree
as a test binary; its own `bootstrap` provisions HyperDX's admin and keys, as the installer's Job does.

```sh
hack/hyperdx-proof/proof.sh            # KEEP=1 keeps the cluster, WORK=dir keeps the run's files
```

Data: OTLP DELTA sums of `traces.span.metrics.calls` through the real OTel ClickHouse exporter,
97 good and 3 failing every 20s, so every window's failing share is exactly 3%. It checks that a
ratio fires at 2.9% and not at 3.1% (sum and increase), that `between [2%, 4%]` fires and
`[4%, 5%]` does not, that an idle 'above' ratio stays OK while a ratio that would fire at 0 is
refused, that HyperDX's totals equal ClickHouse's K and N, the tile's shape, and the CRD's CEL
rules at the real apiserver. Last run: 18 of 18.

Two facts it established about HyperDX 2.35.0: its external `charts/series` API ignores
`seriesReturnType` (the ratio is proven by alert state instead), and a window with no rows is
evaluated as 0.
