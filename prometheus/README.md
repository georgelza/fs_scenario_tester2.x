# prom_wrapper_pg

Push Gateway method...
Metrics specified as part of a struct

- Start Prometheus processes
- Start Prometheus Push Gateway
docker run -p 9091:9091 prom/pushgateway

- Remember to modify the prometheus.yaml to use/reference the push gateway to be scraped.
- Add a hostname/alias to local machine pointing to machine IP.
docker run \
    -p 9090:9090 \
    -v /Users/george/Desktop/ProjectsCommon/fs/fs_scenario_tester2.2/prometheus/config:/etc/prometheus \
    prom/prometheus

- Start Grafana
docker run -p 3000:3000 grafana/grafana-enterprise


Now go into Grafana (default username/password is admin/admin)
Configure a prometheus data source, that uses the above configured hostname,

Do not use 127.0.0.1 as the Grafana container will think of that in a local 
sense and look for the prometheus datastore locally.


Notes
https://prometheus.io/docs/prometheus/latest/installation/

https://prometheus.io/docs/prometheus/latest/getting_started/

https://antonputra.com/monitoring/monitor-golang-with-prometheus/#gauge

