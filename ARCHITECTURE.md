# Arquitetura

> Idioma: esta é a versão padrão em português. A versão em inglês está em
> [ARCHITECTURE.en.md](ARCHITECTURE.en.md).

Serviço distribuído de processamento de apostas em Go, composto com Uber Fx,
apoiado em PostgreSQL, AWS SQS (LocalStack localmente) e Keycloak como provedor
de identidade externo OAuth 2.0/OIDC.

- [1. Visão geral dos componentes](#1-visão-geral-dos-componentes)
- [2. Dinheiro (Money)](#2-dinheiro-money)
- [3. Limites transacionais](#3-limites-transacionais)
- [4. Idempotência](#4-idempotência)
- [5. Controle de concorrência](#5-controle-de-concorrência)
- [6. Máquina de estados da WagerTransaction](#6-máquina-de-estados-da-wagertransaction)
- [7. Operações e reversões](#7-operações-e-reversões)
- [8. Referências pendentes](#8-referências-pendentes)
- [9. Inbox](#9-inbox)
- [10. Outbox transacional](#10-outbox-transacional)
- [11. Autenticação e autorização](#11-autenticação-e-autorização)
- [12. Contrato SQS](#12-contrato-sqs)
- [13. Composição Fx e ciclo de vida](#13-composição-fx-e-ciclo-de-vida)
- [14. Taxonomia de falhas](#14-taxonomia-de-falhas)
- [15. Observabilidade](#15-observabilidade)
- [16. Estratégia de testes](#16-estratégia-de-testes)
- [17. Interpretações, limitações e trabalho não concluído](#17-interpretações-limitações-e-trabalho-não-concluído)

## 1. Visão geral dos componentes

```
                       ┌──────────────────────────────┐
   provedores ─HTTPS──▶│  API HTTP (chi)              │
   interno    ─HTTPS──▶│  authn: Keycloak OIDC/JWKS   │
                       │  authz: papéis + providerId  │
                       └──────────────┬───────────────┘
                                      │
                       ┌──────────────▼───────────────┐
                       │ casos de uso da aplicação    │
                       │  wager.Process / ResumeDue   │
                       │  wallets.Open/Reconcile      │
                       └───────┬──────────────┬───────┘
                               │              │
                 ┌─────────────▼───┐   ┌──────▼────────────────┐
   SQS ──consome─▶│ PostgreSQL      │   │ publicador de outbox  │──publica──▶ SQS
   (inbox)         │ wallets         │   │ resolvedor de refs    │            (eventos)
                 │ wager_tx        │   └───────────────────────┘
                 │ ledger (append) │
                 │ inbox / outbox  │
                 └─────────────────┘
```

Organização dos pacotes:

| Caminho | Responsabilidade |
| --- | --- |
| `internal/domain` | Money, Wallet, WagerTransaction, LedgerEntry, eventos e taxonomia de erros. Sem imports de infraestrutura. |
| `internal/ports` | Interfaces e tipos de valor dos quais a aplicação depende (repositórios, gerenciador de transações, clock, métricas, mensageria). |
| `internal/app` | Casos de uso: `wager`, `wallets`, `outbox`, `references`, `consumer`. |
| `internal/adapters` | Repositórios pgx, cliente/consumidor SQS, verificador OIDC, handlers HTTP. |
| `internal/platform` | Executor de migrations, loop de worker, logging, métricas. |
| `internal/bootstrap` | Módulos Fx e composição do ciclo de vida. |
| `migrations` | SQL versionado, embutido nos binários. |

As camadas de domínio e de aplicação nunca importam pgx, SQS, HTTP ou Fx.

## 2. Dinheiro (Money)

`internal/domain/money` é um value object imutável:

- Representação: `int64` em **unidades mínimas** (centavos) mais um `Currency`
  ISO 4217. Faixa para BRL:
  `[-92_233_720_368_547_758.08, +92_233_720_368_547_758.07]`.
- O parsing é estrito: notação decimal simples, no máximo 2 casas decimais,
  sem notação científica, sem `NaN`/`Infinity`, sem `+`, sem separador de
  milhares e sem espaços em branco. Valores JSON devem ser **strings**; números
  JSON são rejeitados, portanto `float32`/`float64` nunca participam de
  parsing, aritmética, serialização ou persistência.
- O overflow é verificado no parsing, na soma, na subtração e na negação.
- `Parse` aceita valores negativos porque cálculos internos (diferenças,
  movimentos de rollback) precisam deles; entradas financeiras externas passam
  por `ParseNonNegative`/`ParsePositive` (e adicionalmente pelas regras de cada
  tipo).
- Moedas precisam coincidir para aritmética e comparação.
- A serialização é sempre de escala fixa:
  `{"amount":"25.00","currency":"BRL"}`.
- A persistência usa `BIGINT` em unidades mínimas; o ledger armazena saldos na
  mesma representação, então leituras e escritas são exatas.

## 3. Limites transacionais

O acesso ao PostgreSQL usa `pgx` com SQL explícito. O `ports.TxManager` é dono
das transações; os repositórios resolvem a transação atual a partir do contexto
(`postgres.querierFrom`). Chamadas aninhadas de `WithinTransaction` criam
savepoints, o que permite ao consumidor SQS envolver o registro na inbox, as
alterações de domínio e a conclusão da inbox em uma única transação, enquanto o
serviço de apostas ainda faz retries internamente.

Por caso de uso, a unidade atômica é:

| Caso de uso | Transação única |
| --- | --- |
| `wallets.Open` | linha da carteira + transação OPENING + crédito no ledger + 2 eventos de outbox |
| `wager.Process` (BET/WIN/LOSS) | linha da transação + atualização da carteira + lançamento no ledger + eventos de outbox |
| `wager.Process` (REFUND/ROLLBACK resolvido) | linha da transação (+ referência resolvida) + atualização da carteira + lançamento no ledger + eventos de outbox |
| `wager.Process` (referência não resolvida) | linha da transação (`PENDING_REFERENCE`) + evento de pendência na outbox |
| Mensagem SQS | registro na inbox + processamento da aposta + conclusão da inbox |
| publicador de outbox | apenas claim/mark (a publicação ocorre fora da transação, ver §10) |

Nenhum evento é publicado antes do commit da transação que o originou, pois os
eventos só são gravados em `outbox_events` dentro dessa transação e publicados
por um worker separado.

## 4. Idempotência

### Chave e payload

- O HTTP exige o header `Idempotency-Key`. O servidor usa o valor do header
  literalmente e nunca o substitui por `{providerId}:{externalTransactionId}`.
- O SQS usa `data.idempotencyKey` e adicionalmente deduplica pelo `messageId`
  do envelope através da inbox.
- `(providerId, externalTransactionId)` é a identidade financeira: possui
  índice único e não pode ser reaplicada com outra chave ou payload.
- `(providerId, idempotencyKey)` tem seu próprio índice único; reutilizar uma
  chave para outra transação externa é conflito.

### Hash canônico do payload

`wager.ComputePayloadHash` calcula SHA-256 sobre JSON canônico
(`internal/app/wager/hash.go`):

- chaves ordenadas lexicograficamente (marshal de map em Go);
- `amount` normalizado para a string decimal de escala fixa e `currency` para o
  código ISO (portanto `"25"` e `"25.00"` geram o mesmo hash);
- UUIDs em forma canônica minúscula;
- `referenceExternalTransactionId` sempre presente, vazio quando ausente;
- a chave de idempotência e todos os metadados de transporte (headers,
  timestamps, ids de mensagem, ids de correlação) são excluídos.

HTTP e SQS constroem o mesmo `wager.ProcessCommand`, então o hash é idêntico
entre os transportes.

### Resultados

| Situação | Resultado |
| --- | --- |
| Mesma chave, mesmo payload | resultado persistido retornado, `idempotentReplay: true` |
| Mesma transação externa, outra chave, mesmo payload | replay (sem reaplicação) |
| Mesma chave ou transação externa, payload diferente | `409 IDEMPOTENCY_CONFLICT` |
| Mesmo `messageId`, mesmo corpo (SQS) | linha da inbox concluída, mensagem confirmada, sem reaplicação |
| Mesmo `messageId`, corpo diferente | falha permanente → DLQ |

O replay retorna o saldo observado no processamento original, persistido em
`wager_transactions.result_balance_minor`, e não o saldo atual da carteira.

## 5. Controle de concorrência

A coordenação é **por carteira**, com controle de concorrência otimista:

```sql
UPDATE wallets
SET balance_minor = $2, version = version + 1, updated_at = $3
WHERE id = $1 AND version = $4
```

- Zero linhas afetadas significa que outro escritor venceu; a operação é
  repetida a partir de uma leitura nova, limitada por `MaxWalletRetries`
  (padrão 5).
- Débitos são validados contra o agregado em memória **e** pela constraint
  `wallets_balance_non_negative`, então um lost update nunca pode produzir saldo
  negativo mesmo se a lógica da aplicação estivesse errada.
- `wallet_ledger_entries (wallet_id, transaction_id)` é único, então uma
  transação movimenta uma carteira no máximo uma vez. O ledger é append-only:
  triggers rejeitam `UPDATE`, `DELETE` e `TRUNCATE`.
- Violações de unicidade abortam a transação do PostgreSQL; o loop de retry
  inicia uma nova transação e relê o vencedor já commitado (por isso inserts
  duplicados também são erros retentáveis, ver `wager.isRetryable`).
- Não há lock global. Carteiras diferentes atualizam linhas independentes e são
  processadas em paralelo.

Corridas de reversão são adicionalmente serializadas pela nova verificação de
`FindReversal` após o conflito de versão da carteira e pelo índice único
parcial `wager_tx_reference_reversal_unique (reference_transaction_id, kind)
WHERE status = 'PROCESSED'`.

## 6. Máquina de estados da WagerTransaction

```
            ┌─────────┐
            │ PENDING │────────────┐
            └────┬────┘            │
                 │                 │
   referência    │                 │  sucesso síncrono
   indisponível  ▼                 ▼
      ┌────────────────────┐   ┌───────────┐
      │ PENDING_REFERENCE  │──▶│ PROCESSED │
      └─────────┬──────────┘   └───────────┘
                │              ┌──────────┐
                ├─────────────▶│ REJECTED │
                │              └──────────┘
                │              ┌────────┐
                └─────────────▶│ FAILED │
                               └────────┘
```

- `PENDING`: aceita, ainda não concluída. Novas operações são inseridas e
  concluídas na mesma transação, então um `PENDING` commitado só é possível se
  uma aceitação assíncrona explícita for introduzida. O worker de recuperação
  ainda reivindica linhas `PENDING` antigas, atendendo ao requisito de retomada
  durável.
- `PENDING_REFERENCE`: aguardando uma referência; repetida com backoff
  exponencial pelo resolvedor de referências.
- `PROCESSED`, `REJECTED`, `FAILED`: terminais. O `Update` só toca linhas com
  status `PENDING`/`PENDING_REFERENCE`, e o trigger de banco
  `wager_transactions_no_terminal_update` rejeita qualquer `UPDATE` em linha
  terminal, então os estados terminais são imutáveis nas duas camadas.
- As transições são validadas por `wagering.assertTransition`; `panic` nunca é
  usado para rejeições de negócio.
- Transitório vs permanente: erros de banco/broker são repetidos (visibilidade
  do SQS, backoff da outbox). Rejeições de negócio são persistidas com
  `failure_code` estável; falhas permanentes de infraestrutura seriam
  registradas como `FAILED` (o caminho existe e tem teste unitário, mas nenhum
  adapter atual classifica um erro como `FAILED` permanente — ver §17).

## 7. Operações e reversões

| Tipo | Movimentação | Regras |
| --- | --- | --- |
| `BET` | DÉBITO | valor positivo, saldo suficiente |
| `WIN` | CRÉDITO | valor positivo, referência opcional a uma BET da mesma rodada |
| `LOSS` | nenhuma | `money.amount == "0.00"`; sem ledger, sem incremento de versão; emite `WagerTransactionProcessed` |
| `REFUND` | CRÉDITO | referencia uma `BET` `PROCESSED`, mesmo valor |
| `ROLLBACK` | oposta à da referência | referencia `BET` (crédito), `WIN` (débito) ou `REFUND` (débito), mesmo valor |

A resolução de referência exige concordância de provedor, jogador, carteira,
moeda e rodada. Valores de reversão devem ser iguais ao valor referenciado;
reversões parciais estão fora do escopo.

Matriz de conflitos de reversão (cada linha é rejeitada com
`failureCode = REVERSAL_CONFLICT`):

| Já bem-sucedida | Nova tentativa | Permitido? |
| --- | --- | --- |
| REFUND de BET | REFUND da mesma BET | não |
| ROLLBACK de BET | ROLLBACK da mesma BET | não |
| REFUND de BET | ROLLBACK dessa BET | não (devolveria o débito duas vezes) |
| ROLLBACK de BET | REFUND dessa BET | não |
| qualquer reversão | ROLLBACK dessa reversão | não |
| REFUND de BET | ROLLBACK desse REFUND | sim (desfaz o crédito) |

Um rollback que precisaria debitar mais que o saldo disponível é rejeitado com
`REVERSAL_INSUFFICIENT_FUNDS`, distinto de `INSUFFICIENT_FUNDS` de uma aposta.

`OPENING` é exclusivamente interno: HTTP e SQS o rejeitam (400 / DLQ). Ele é
criado junto com a carteira e sempre `PROCESSED`.

## 8. Referências pendentes

Quando uma reversão (ou um WIN com referência) aponta para uma transação
desconhecida ou em andamento:

1. A operação é commitada como `PENDING_REFERENCE` com `attempts = 1` e
   `next_attempt_at = now + backoff`.
2. `WagerTransactionPendingReference` é gravado na outbox.
3. O resolvedor de referências (`references.Resolver`) reivindica linhas
   vencidas com `SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1` e as retoma.
4. Quando uma transação é processada com sucesso, `WakeDependents` define
   `next_attempt_at = now()` para todas as operações que a aguardam, então a
   resolução normalmente é imediata em vez de esperar o próximo tick de
   backoff.
5. O backoff é exponencial (`base * 2^(attempt-1)`, com teto). Após
   `REFERENCE_MAX_ATTEMPTS` a operação é rejeitada com `REFERENCE_NOT_FOUND` e
   `WagerTransactionRejected` é emitido.
6. Se a referência existe mas é terminal sem sucesso (`REJECTED`/`FAILED`), a
   operação é rejeitada imediatamente com `REFERENCE_NOT_PROCESSED`.
7. Se a referência ainda está `PENDING`/`PENDING_REFERENCE`, a espera continua.

Tudo isso sobrevive a reinícios: o estado está no PostgreSQL e o worker varre em
todas as instâncias.

## 9. Inbox

`inbox_messages (consumer_name, message_id)` é único. O handler SQS executa, em
uma única transação:

1. `INSERT ... ON CONFLICT DO NOTHING RETURNING id` (claim);
2. alterações de domínio através do serviço de apostas;
3. `UPDATE ... SET status = 'COMPLETED'`.

Consequências:

- Reentrega após uma queda entre o commit e o `DeleteMessage` encontra a linha
  e é ignorada; a mensagem então é deletada.
- Reentrega com corpo diferente para o mesmo `messageId` é falha permanente
  roteada para a DLQ.
- Uma referência pendente é commitada junto com a conclusão da inbox, então a
  mensagem é confirmada e o resolvedor assume.

## 10. Outbox transacional

`outbox_events` armazena um snapshot JSONB imutável do envelope completo do
evento. O publicador (`outbox.Publisher`):

1. reivindica um lote em uma transação curta (`FOR UPDATE SKIP LOCKED`, TTL de
   lock);
2. publica no SQS fora da transação;
3. marca o evento como publicado (ou reagenda com backoff em caso de falha).

A ordem por agregado é preservada ao reivindicar apenas o evento não publicado
mais antigo de cada agregado, mesmo que outro publicador detenha seu lock.
Locks abandonados são recuperados após `OUTBOX_LOCK_TTL`. Uma queda entre a
publicação e a confirmação causa republicação com o **mesmo `eventId`**;
consumidores deduplicam por `eventId`.

Envelope do evento:

```json
{
  "eventId": "01a0...",
  "eventType": "WalletBalanceChanged",
  "aggregateId": "01a0...",
  "correlationId": "id da requisição ou da mensagem",
  "causationId": "transactionId",
  "occurredAt": "2026-09-24T19:33:37.424Z",
  "version": 1,
  "data": { "...": "tipado por evento" }
}
```

`WalletBalanceChanged.data` contém `walletId`, `transactionId`, `direction`,
`money`, `balanceBefore`, `balanceAfter`, `walletVersion`. Timestamps são UTC
RFC 3339 e valores são strings decimais.

## 11. Autenticação e autorização

- Descoberta OIDC + verificação JWKS via `coreos/go-oidc` contra o Keycloak.
  Issuer e audience são validados; tokens ausentes, malformados ou expirados
  são rejeitados com 401. Não há armazenamento de senhas nem emissão de tokens
  no serviço.
- Papéis de realm: `provider` (provedores externos) e `internal` (serviço de
  carteiras).
- A identidade autenticada determina o `providerId` autorizado através da claim
  `provider_id` (um hardcoded claim mapper por cliente Keycloak).
- Provedores só podem enviar operações para o próprio `providerId` e ler as
  próprias transações (por id interno ou id externo); qualquer outro provedor
  recebe 403 e nunca vê dados.
- Criação de carteira, leitura de carteira, leitura do ledger e reconciliação
  exigem o papel `internal`. `OPENING` é adicionalmente rejeitado no nível de
  domínio.
- Health checks e `/metrics` são públicos.
- O acesso ao SQS é controlado por credenciais do broker; o consumidor ainda
  aplica todas as validações de domínio, então uma credencial válida do broker
  não pode burlar regras de negócio.

Realm, clients, secrets e papéis de service account do Keycloak são
provisionados automaticamente a partir de `deploy/keycloak/realm.json`.

## 12. Contrato SQS

| Fila | Propósito | MessageGroupId | MessageDeduplicationId |
| --- | --- | --- | --- |
| `wager-transactions.fifo` | operações de entrada | `walletId` (ordem por carteira) | `idempotencyKey` |
| `wager-transactions-dlq.fifo` | redrive após 5 recebimentos ou roteamento explícito para DLQ | `dlq` | `dlq-<messageId>-<nano>` |
| `wager-events.fifo` | eventos de integração da outbox | `aggregateId` (ordem por carteira) | `eventId` (estável entre republicações) |
| `wager-events-dlq.fifo` | DLQ dos eventos de outbox | — | — |

- Long polling, `SQS_VISIBILITY_TIMEOUT` (padrão 30s), `maxReceiveCount = 5`.
- A mensagem só é deletada após o handler confirmar o commit durável.
- Rejeições de negócio são resultados duráveis e são confirmadas (deletadas).
- Mensagens inválidas ou não processáveis são movidas explicitamente para a
  DLQ e deletadas; falhas transitórias ficam para reentrega e chegam à DLQ pela
  política de redrive.
- Em `SIGTERM` o consumidor para de buscar e conclui a mensagem em andamento
  dentro do período de graça do shutdown; se não conseguir, o visibility
  timeout expira e a mensagem é reentregue. A inbox torna a reentrega segura.

Envelope de entrada (ver `internal/app/consumer/wager.go`):

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "...",
    "walletId": "...",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" },
    "referenceExternalTransactionId": "optional"
  }
}
```

## 13. Composição Fx e ciclo de vida

`internal/bootstrap/app.go` define um `fx.Module` por camada: `config`,
`observability`, `postgres`, `messaging`, `auth`, `app`, `http` e `workers`.
Construtores recebem suas dependências explicitamente; interfaces são
fornecidas com pequenas funções adaptadoras para que `ports` permaneça livre de
framework.

Ciclo de vida:

- **Start**: a configuração é validada por `config.Load`; as migrations rodam
  como um `fx.Invoke` antes de qualquer hook; o servidor HTTP, o consumidor SQS
  e os dois workers iniciam em hooks `OnStart`.
- **Stop** (LIFO): o HTTP para de aceitar conexões e drena requisições em
  andamento (`http.Server.Shutdown`); o consumidor SQS para de buscar e termina
  a mensagem atual; o publicador de outbox e o resolvedor de referências param
  seus loops; por fim o pool pgx é fechado (`fx.OnStop` no provider do pool,
  registrado primeiro para executar por último).
- O timeout de stop é 30s (`fx.StopTimeout`), e o consumidor SQS usa o timeout
  de shutdown do HTTP como período de graça de processamento.
- Eventos `fxevent` são roteados para o logger estruturado.

## 14. Taxonomia de falhas

Erros de domínio carregam um `kind` (mapeamento de transporte) e um `code`
estável (`internal/domain/apperr`). Mapeamento HTTP:

| Kind | HTTP | Exemplos |
| --- | --- | --- |
| invalid | 400 | JSON malformado, UUID inválido, kind inválido, chave ausente |
| unauthorized | 401 | token ausente/inválido/expirado |
| forbidden | 403 | provedor acessando outro provedor |
| not_found | 404 | carteira ou transação desconhecida |
| conflict | 409 | conflito de idempotência, carteira duplicada |
| rejected | 422 | rejeição de negócio com `failureCode` |
| unavailable/internal | 503/500 | infraestrutura transitória |

Códigos de falha de negócio (persistidos, retornados em respostas e eventos):

| Código | Significado | Corrigível |
| --- | --- | --- |
| `INSUFFICIENT_FUNDS` | débito de BET excede o saldo | sim (após depósito) |
| `REVERSAL_INSUFFICIENT_FUNDS` | débito de rollback excede o saldo | sim |
| `REFERENCE_NOT_FOUND` | referência nunca chegou (tentativas esgotadas) | sim (nova requisição) |
| `REFERENCE_NOT_PROCESSED` | referência é terminal sem sucesso | não |
| `REFERENCE_MISMATCH` | referência discorda em jogador/carteira/moeda/rodada/kind | não |
| `REFERENCE_AMOUNT_MISMATCH` | valor da reversão difere | não |
| `REVERSAL_CONFLICT` | reversão duplicada ou cruzada | não |
| `WALLET_NOT_FOUND` | carteira não existe | sim (após criação) |
| `WALLET_PLAYER_MISMATCH` | carteira pertence a outro jogador | não |
| `CURRENCY_MISMATCH` | moedas da carteira e da operação diferem | não |
| `INVALID_AMOUNT` | zero/negativo onde positivo é exigido | sim |
| `UNSUPPORTED_KIND` | tipo de operação não suportado | não |

`GET /wagering/transactions/:id` retorna `failureCorrectable` para distinguir
resultados corrigíveis de definitivos.

## 15. Observabilidade

- Logs JSON (`slog`) com `requestId`, `messageId`, `transactionId`, `walletId` e
  `providerId` quando disponíveis. Nenhuma credencial ou payload financeiro
  completo é logado.
- Métricas Prometheus em `/metrics`: resultados por status/kind/código de falha,
  replays idempotentes, conflitos de versão da carteira, retries de workers,
  mensagens em DLQ, resultados de reconciliação, backlog e latência de
  publicação da outbox, transações pendentes, contadores e latência HTTP.
- `GET /health/live` (processo) e `GET /health/ready` (PostgreSQL + SQS).
- Tracing OpenTelemetry e dashboards não foram implementados (opcionais).

## 16. Estratégia de testes

- **Unitários** (`go test ./...`): parsing/escala/overflow/incompatibilidade de
  moeda do Money, invariantes da carteira, máquina de estados da transação e
  regras por tipo, matemática do ledger, construtores de eventos e hash
  canônico.
- **Integração** (`-tags=integration`, com PostgreSQL/Keycloak/LocalStack
  reais): migrations, constraints, imutabilidade do ledger, imutabilidade de
  transações terminais, fluxos financeiros, replay/conflito de idempotência,
  dedup de inbox e roteamento para DLQ, reentrega SQS após queda simulada entre
  commit e delete, concorrência de outbox, retry, recuperação de lock
  abandonado e republicação com eventId estável, resolução e expiração de
  referências pendentes, preservação após restart, start/stop da composição Fx,
  autenticação HTTP (tokens ausentes, malformados, forjados — `alg: none`,
  payload adulterado, chave de assinatura errada, issuer errado, audience
  errada — e expirados), a matriz de autorização (isolamento entre provedores,
  operações de carteira restritas ao serviço interno, health/metrics públicos)
  e a ausência de efeitos colaterais em tentativas não autorizadas, além dos
  cenários obrigatórios de concorrência (50× a mesma aposta, duas apostas de
  80.00 sobre 100.00, carteiras distintas em paralelo) usando três instâncias
  independentes (pools separados).
- Os **unitários** cobrem adicionalmente a semântica de conflito de
  idempotência com fakes de repositório em memória
  (`internal/app/wager/service_test.go`).
- `go test -race` é executado para as suítes unitária e de integração.

## 17. Interpretações, limitações e trabalho não concluído

Interpretações adotadas:

- `idempotencyKey` é obrigatória nos dois transportes (o desafio torna o header
  HTTP obrigatório e a exibe no envelope SQS).
- A escala é fixa em duas casas decimais para toda moeda.
- Operações rejeitadas que não podem referenciar uma carteira existente
  (`walletId` desconhecido) não são persistidas, porque
  `wager_transactions.wallet_id` é uma foreign key. Elas retornam
  `422 WALLET_NOT_FOUND` e vão para a DLQ no caminho SQS. Todas as demais
  rejeições são persistidas e auditáveis.
- LOSS sucede sem alteração de saldo e armazena o saldo observado como
  resultado; `WalletBalanceChanged` não é emitido.
- Para eventos originados de uma transação retomada, o `correlationId` cai para
  o id da transação, já que a correlação original do transporte não é
  armazenada no agregado.
- A referência de WIN é opcional; quando presente, deve ser uma `BET`
  `PROCESSED` da mesma rodada, e o valor do WIN pode diferir do valor da
  aposta.
- Referências de reversão devem corresponder à rodada original; isso é mais
  estrito que "mesmo jogador/carteira/moeda", mas corresponde ao texto do
  domínio.

Limitações conhecidas / trabalho não concluído:

- `FAILED` é modelado, persistido e testado em unidade, mas nenhum adapter
  atualmente classifica um erro como falha permanente de infraestrutura; erros
  transitórios são sempre repetidos. Um deployment de produção adicionaria um
  classificador para erros não retentáveis de broker/banco.
- Eventos de outbox são repetidos indefinidamente com backoff limitado (eventos
  financeiros críticos não podem ser descartados). Não há um switch
  administrativo de "enviar para DLQ após N tentativas" no publicador de
  outbox; a DLQ da fila de eventos cobre falhas do lado do broker.
- Sem artefatos de teste de carga (diferencial opcional).
- Sem tracing OpenTelemetry ou dashboards (diferencial opcional).
- Ledger de partidas dobradas não implementado (diferencial opcional).
