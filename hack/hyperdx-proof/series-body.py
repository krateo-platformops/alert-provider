"""The charts/series request for the failing and total calls over [start, end), one 5m bucket."""
import json
import sys

src, s, e = sys.argv[1], int(sys.argv[2]) * 1000, int(sys.argv[3]) * 1000


def ser(where):
    return {"sourceId": src, "aggFn": "sum", "metricName": "traces.span.metrics.calls", "metricDataType": "sum",
            "where": where, "whereLanguage": "sql", "groupBy": []}


print(json.dumps({"series": [ser("Attributes['krateo.sli.error'] = 'true' AND Attributes['service.name'] = 'snowplow'"),
                             ser("Attributes['service.name'] = 'snowplow'")],
                  "startTime": s, "endTime": e, "granularity": "5m"}))
