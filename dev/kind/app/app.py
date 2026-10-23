"""A stand-in for vLLM: sends the metrics the otelcov fixtures use, over OTLP.

- process.memory.usage (gauge, By): this process's resident memory
- debug.heartbeat (gauge): noise that the gateway's filter drops
- vllm.requests (counter): a fake request count; exported as delta when
  OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta, so the gateway's
  deltatocumulative has work to do

Everything else comes from the standard OTEL_* environment variables (see
k8s/apps.yaml): endpoint, export interval, service.name and resource
attributes.
"""

import os
import random
import signal
import sys
import time

from opentelemetry import metrics
from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
from opentelemetry.metrics import Observation
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader

PAGE_SIZE = os.sysconf("SC_PAGE_SIZE")


def resident_memory(_options):
    with open("/proc/self/statm") as f:
        resident_pages = int(f.read().split()[1])
    yield Observation(resident_pages * PAGE_SIZE)


def heartbeat(_options):
    yield Observation(1)


def main():
    # Export interval: OTEL_METRIC_EXPORT_INTERVAL. Resource: OTEL_SERVICE_NAME
    # and OTEL_RESOURCE_ATTRIBUTES.
    reader = PeriodicExportingMetricReader(OTLPMetricExporter())
    provider = MeterProvider(metric_readers=[reader])
    metrics.set_meter_provider(provider)

    def stop(*_):
        provider.shutdown()  # flushes the last export
        sys.exit(0)

    signal.signal(signal.SIGTERM, stop)

    meter = metrics.get_meter("otelcov.demo")
    meter.create_observable_gauge(
        "process.memory.usage", callbacks=[resident_memory], unit="By",
        description="Resident memory of the process")
    meter.create_observable_gauge(
        "debug.heartbeat", callbacks=[heartbeat],
        description="Noise the gateway drops")
    requests = meter.create_counter(
        "vllm.requests", unit="{request}", description="Requests served (fake)")

    while True:
        requests.add(random.randint(1, 5))
        time.sleep(1)


if __name__ == "__main__":
    main()
