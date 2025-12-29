# dns-policy-router (OpenShift / multi-site / sem service mesh)

Este projeto implementa um DNS interno no cluster (OpenShift) que permite que microserviços
consumam sempre o mesmo hostname canônico, por exemplo:

- `orders.corp.com`

Mas, por trás, o DNS escolhe o endpoint "mais próximo" (preferido) conforme a política do cluster,
tentando nomes prefixados em uma ordem definida via `CLUSTER_LIST`, por exemplo:

- `b.orders.corp.com`
- `aro.orders.corp.com`
- `a.orders.corp.com`

A fonte da verdade continua sendo o **DNS corporativo**. Este componente apenas:
1) Decide a ordem de tentativa (policy)
2) Resolve no DNS corporativo
3) Cacheia respostas em Redis

---

## Como funciona

### Convenção no DNS corporativo (já provisionada pelo seu Operator)
Para cada microserviço canônico `orders.corp.com`, existem registros prefixados:

- `a.orders.corp.com` -> IP do LB/Ingress do cluster A
- `b.orders.corp.com` -> IP do LB/Ingress do cluster B
- `aro.orders.corp.com` -> IP do LB/Ingress do ARO

O DNS corporativo não precisa ter lógica de prioridade.

### A lógica (policy) fica no cluster
Cada cluster sobe o `dns-policy-router` com uma ordem diferente:

- Cluster A: `CLUSTER_LIST="a,b,aro"`
- Cluster B: `CLUSTER_LIST="b,aro,a"`
- Cluster ARO: `CLUSTER_LIST="aro,a,b"`

Quando um pod resolve `orders.corp.com`, o `dns-policy-router`:
1) Detecta que o nome está na zona `corp.com`
2) Detecta que é canônico (não prefixado)
3) Gera candidatos na ordem configurada:
   - `b.orders.corp.com`, depois `aro.orders.corp.com`, depois `a.orders.corp.com`
4) Consulta o DNS corporativo (UPSTREAMS) e retorna o primeiro que der resposta útil
5) Cacheia no Redis (respeitando TTL e um cap `CACHE_MAX_TTL`)

Se a query já for prefixada (ex: `b.orders.corp.com`), ele só faz passthru.

---

## Variáveis de ambiente

Obrigatórias:
- `UPSTREAMS` (ex: `10.10.10.10:53,10.10.10.11:53`)
- `CLUSTER_LIST` (ex: `b,aro,a`)
- `ZONE` (ex: `corp.com.`)

Recomendadas:
- `LISTEN_ADDR` (default `:1053`)
- `REDIS_ADDR` (default `redis-dns-cache:6379`)
- `CACHE_MAX_TTL` (default `60s`) -> cap do TTL armazenado no Redis
- `NEG_CACHE_TTL` (default `15s`) -> cache de NXDOMAIN/empty
- `QUERY_TIMEOUT` (default `900ms`)
- `ALLOW_PASSTHRU` (default `true`)
- `LOG_DECISIONS` (default `true`)

Opcional:
- `TLS_UPSTREAM` (default `false`)
- `TLS_SERVER_NAME`

---

## Como plugar no OpenShift

A ideia é o CoreDNS/openshift-dns encaminhar a zona `corp.com` para este serviço
(isto é, forward condicional por zona). Exemplo conceitual:

- Queries para `*.corp.com` -> `dns-policy-router.<ns>.svc.cluster.local`
- O restante continua resolvido normalmente (cluster.local etc.)

Importante: rode o container em `1053` e exponha Service na porta 53.

---

## Observações de produção

- O cache Redis evita sobrecarga no DNS corporativo.
- TTL baixo (30-60s) ajuda failover rápido sem “congelar” escolhas ruins.
- Sem mesh: resiliência depende também de timeouts/retries no cliente HTTP.
- O componente suporta UDP e TCP (DNS) e faz fallback para TCP se truncado.

---

## Build e run local (opcional)

```bash
go build -o dns-policy-router main.go

export ZONE="corp.com."
export CLUSTER_LIST="b,aro,a"
export UPSTREAMS="10.10.10.10:53"
export REDIS_ADDR="localhost:6379"
export LISTEN_ADDR=":1053"

./dns-policy-router
