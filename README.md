# dns-policy-router

[![Go](https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![OpenShift](https://img.shields.io/badge/OpenShift-multi--site-EE0000?logo=redhatopenshift&logoColor=white)](https://www.openshift.com/)
[![Redis](https://img.shields.io/badge/cache-Redis-DC382D?logo=redis&logoColor=white)](https://redis.io/)

**DNS interno com política de proximidade para clusters OpenShift multi-site, sem depender de service mesh.**

Permite que microserviços resolvam sempre o mesmo hostname canônico (ex.: `orders.corp.com`) enquanto o `dns-policy-router` escolhe, por trás dos panos, o endpoint mais adequado para o cluster em que está rodando — com cache em Redis e fallback automático para o DNS corporativo.

---

## Sumário

- [Como funciona](#como-funciona)
- [Arquitetura](#arquitetura)
- [Variáveis de ambiente](#variáveis-de-ambiente)
- [Como plugar no OpenShift](#como-plugar-no-openshift)
- [Build e execução local](#build-e-execução-local)
- [Build da imagem de container](#build-da-imagem-de-container)
- [Observações de produção](#observações-de-produção)
- [Estrutura do projeto](#estrutura-do-projeto)
- [Licença](#licença)

---

## Como funciona

A fonte da verdade continua sendo o **DNS corporativo**. Este componente apenas:

1. Decide a ordem de tentativa (*policy*) por cluster;
2. Resolve o hostname prefixado correspondente no DNS corporativo;
3. Cacheia a resposta em Redis (respeitando TTL, com um teto configurável).

### Convenção no DNS corporativo (já provisionada pelo seu Operator)

Para cada microserviço canônico `orders.corp.com`, existem registros prefixados:

| Registro | Aponta para |
|---|---|
| `a.orders.corp.com` | IP do LB/Ingress do cluster A |
| `b.orders.corp.com` | IP do LB/Ingress do cluster B |
| `aro.orders.corp.com` | IP do LB/Ingress do ARO |

O DNS corporativo não precisa ter lógica de prioridade — essa lógica fica inteiramente no cluster.

### A política fica no cluster

Cada cluster sobe o `dns-policy-router` com uma ordem diferente, via `CLUSTER_LIST`:

| Cluster | `CLUSTER_LIST` |
|---|---|
| A | `a,b,aro` |
| B | `b,aro,a` |
| ARO | `aro,a,b` |

Quando um pod resolve `orders.corp.com`, o `dns-policy-router`:

1. Detecta que o nome está na zona `corp.com`;
2. Detecta que é canônico (não prefixado);
3. Gera candidatos na ordem configurada — ex.: `b.orders.corp.com`, depois `aro.orders.corp.com`, depois `a.orders.corp.com`;
4. Consulta o DNS corporativo (`UPSTREAMS`) e retorna o primeiro que der resposta útil;
5. Cacheia a resposta no Redis (respeitando TTL e o cap `CACHE_MAX_TTL`).

Se a query já vier prefixada (ex.: `b.orders.corp.com`), o serviço apenas faz *passthru*.

## Arquitetura

```
                    ┌─────────────────────┐
 pod → orders.corp.com │  dns-policy-router  │
                    │  (CoreDNS forward)  │
                    └──────────┬──────────┘
                               │ 1. resolve na ordem do CLUSTER_LIST
                               ▼
                    ┌─────────────────────┐
                    │   Redis (cache)     │◄── 3. cache com TTL
                    └──────────┬──────────┘
                               │ 2. cache miss
                               ▼
                    ┌─────────────────────┐
                    │   DNS corporativo    │
                    │   (UPSTREAMS)        │
                    └─────────────────────┘
```

O componente fala DNS nativo (UDP/TCP, com fallback para TCP em respostas truncadas), então basta um *forward* condicional por zona no CoreDNS/openshift-dns.

## Variáveis de ambiente

**Obrigatórias**

| Variável | Exemplo | Descrição |
|---|---|---|
| `UPSTREAMS` | `10.10.10.10:53,10.10.10.11:53` | Servidores DNS corporativos consultados na resolução |
| `CLUSTER_LIST` | `b,aro,a` | Ordem de preferência dos prefixos de cluster |
| `ZONE` | `corp.com.` | Zona canônica atendida por este serviço |

**Recomendadas**

| Variável | Default | Descrição |
|---|---|---|
| `LISTEN_ADDR` | `:1053` | Endereço/porta onde o serviço escuta |
| `REDIS_ADDR` | `redis-dns-cache:6379` | Endereço do Redis usado como cache |
| `CACHE_MAX_TTL` | `60s` | Teto do TTL armazenado no Redis |
| `NEG_CACHE_TTL` | `15s` | TTL do cache negativo (NXDOMAIN/vazio) |
| `QUERY_TIMEOUT` | `900ms` | Timeout por consulta ao upstream |
| `ALLOW_PASSTHRU` | `true` | Permite passthru de nomes já prefixados |
| `LOG_DECISIONS` | `true` | Loga a decisão de roteamento tomada por query |

**Opcionais**

| Variável | Default | Descrição |
|---|---|---|
| `TLS_UPSTREAM` | `false` | Usa DNS-over-TLS ao consultar os upstreams |
| `TLS_SERVER_NAME` | — | SNI usado no handshake TLS com o upstream |

## Como plugar no OpenShift

A ideia é o CoreDNS/openshift-dns encaminhar a zona `corp.com` para este serviço (*forward* condicional por zona). Conceitualmente:

- Queries para `*.corp.com` → `dns-policy-router.<ns>.svc.cluster.local`
- O restante continua resolvido normalmente (`cluster.local`, etc.)

> **Importante:** o container escuta em `1053`; o `Service` expõe a porta `53`.

Manifests prontos em [`openshift/`](openshift/):

```bash
oc apply -f openshift/redis.yaml
oc apply -f openshift/deployment.yaml
```

- [`openshift/deployment.yaml`](openshift/deployment.yaml) — `ConfigMap`, `Deployment` (2 réplicas, probes de liveness/readiness) e `Service` do `dns-policy-router`.
- [`openshift/redis.yaml`](openshift/redis.yaml) — `Deployment` e `Service` do Redis usado como cache.

## Build e execução local

Requisitos: Go 1.22+ e, opcionalmente, um Redis acessível localmente.

```bash
go build -o dns-policy-router main.go

export ZONE="corp.com."
export CLUSTER_LIST="b,aro,a"
export UPSTREAMS="10.10.10.10:53"
export REDIS_ADDR="localhost:6379"
export LISTEN_ADDR=":1053"

./dns-policy-router
```

Teste rápido com `dig`:

```bash
dig @127.0.0.1 -p 1053 orders.corp.com
```

## Build da imagem de container

O [`Dockerfile`](Dockerfile) faz build multi-stage (Go 1.22 alpine → `distroless/static`), gerando uma imagem final mínima, sem shell, rodando como usuário não-root:

```bash
docker build -t dns-policy-router:latest .
docker run --rm -p 1053:1053/udp -p 1053:1053/tcp \
  -e ZONE="corp.com." \
  -e CLUSTER_LIST="b,aro,a" \
  -e UPSTREAMS="10.10.10.10:53" \
  dns-policy-router:latest
```

## Observações de produção

- O cache Redis evita sobrecarga no DNS corporativo.
- TTL baixo (30–60s) ajuda failover rápido sem "congelar" escolhas ruins.
- Sem service mesh: a resiliência depende também de timeouts/retries no cliente HTTP.
- O componente suporta UDP e TCP (DNS) e faz fallback para TCP quando a resposta é truncada.

## Estrutura do projeto

```
.
├── main.go                  # Servidor DNS, política de roteamento e cache Redis
├── go.mod / go.sum          # Dependências (miekg/dns, redis/go-redis)
├── Dockerfile                # Build multi-stage → imagem distroless
└── openshift/
    ├── deployment.yaml       # ConfigMap + Deployment + Service do router
    └── redis.yaml            # Deployment + Service do Redis de cache
```

## Licença

Este repositório ainda não define uma licença explícita. Entre em contato com o autor ([@hericos](https://github.com/hericos)) antes de reutilizar o código em outros projetos.
