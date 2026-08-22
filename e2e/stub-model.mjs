/*
A stand-in for the customer's Ollama, for the browser tests.

The agentic screens cannot be exercised without something on the other end of
the model call, and the real thing is the wrong tool for the job here: a browser
test that depended on a network model would be slow, would cost money, and would
fail for reasons that have nothing to do with the interface. What these tests
check is what the screens do with a trace, so a scripted endpoint is exactly the
right fidelity.

It speaks the OpenAI chat-completions dialect, which is what `veritix serve`
talks to any local runtime with, so the provider, the loop and the egress guard
are all on the tested path — only the model's judgment is faked.

Run:  node stub-model.mjs [port]
*/

import { createServer } from 'node:http'

const port = Number(process.argv[2] ?? 11435)

/*
The script. Each entry is one reply, in order; after the last one the model
says it has finished, which ends the loop.

The finding is real: the fixture's orders.csv records one order at -20.00, so
the engine reproduces it and it reaches the report. The first attempt to record
it claims 400 rows, which is refused — that exchange is the mechanism the whole
design rests on, and having it in the browser tests means the screens are
exercised against a run where the model got something wrong, which is the
normal case rather than the exceptional one.

The proposal is real in the same way. customers.csv holds a small fixed
vocabulary of statuses, one of them spelled "Actve", and a one_of rule is
exactly how that becomes catchable on every future audit. The model cannot
write the value list — it has never seen a cell value — so it proposes the
shape and Veritix fills the permitted set in from the column, which is the
thing the accept screen exists to show somebody before they bless it.
*/
const finding = {
  rule: 'negative_order_amount',
  severity: 'error',
  table: 'orders_csv',
  column: 'amount',
  detail:
    'A refund entered as an order will be added to revenue instead of subtracted from it, ' +
    'so every total built on this column is wrong by twice the refunded value.',
  remedy: 'Move refunds to their own table, or add a sign convention the loader understands.',
  count_query: 'SELECT count(*) FROM orders_csv WHERE TRY_CAST(amount AS DOUBLE) < 0',
  row_query: 'SELECT * FROM orders_csv WHERE TRY_CAST(amount AS DOUBLE) < 0',
  expected: 'every order amount to be zero or positive',
  observed: 'amounts below zero',
}

const script = [
  toolCall('list_tables', {}),
  toolCall('describe_table', { table: 'orders_csv' }),
  toolCall('record_finding', {
    ...finding,
    title: '400 orders are recorded with a negative amount',
    affected_count: 400,
  }),
  toolCall('record_finding', {
    ...finding,
    title: '1 order is recorded with a negative amount',
    affected_count: 1,
  }),
  toolCall('propose_rule', {
    rule: 'status_domain',
    description: 'customer status has to be one of the values in use today',
    rationale:
      'The column holds a handful of values that repeat, so it is a vocabulary rather than free ' +
      'text. A spelling that appears once is a typing mistake, and nothing in the file will ' +
      'notice it.',
    table: 'customers_csv',
    column: 'status',
    expect: 'one_of',
    severity: 'error',
    // Nothing breaks a vocabulary drawn from the column itself, which is the
    // good case rather than a claim that failed to reproduce.
    violations_now: 0,
  }),
]

function toolCall(name, args) {
  return {
    model: 'stub-model',
    choices: [
      {
        finish_reason: 'tool_calls',
        message: {
          content: '',
          tool_calls: [
            {
              id: `call-${name}`,
              type: 'function',
              function: { name, arguments: JSON.stringify(args) },
            },
          ],
        },
      },
    ],
    usage: { prompt_tokens: 1200, completion_tokens: 90 },
  }
}

const done = {
  model: 'stub-model',
  choices: [
    {
      finish_reason: 'stop',
      message: {
        content:
          'I looked at the order amounts and recorded one finding: a negative amount that ' +
          'will be added to revenue rather than subtracted from it.',
      },
    },
  ],
  usage: { prompt_tokens: 1400, completion_tokens: 40 },
}

/*
Where in the script a request lands is read from the request itself, rather than
counted here.

A counter on the server is state shared between audits, and the suite runs more
than one: the second run would start at the end of the script and be told the
model had finished before it had done anything, which presents as an agent that
produced nothing rather than as a stub that ran out. The transcript already
carries the answer — every reply the model has given in this conversation is in
it — so each request decides for itself, and any number of runs in any order get
the same script from the beginning.
*/
function replyTo(body) {
  const messages = Array.isArray(body?.messages) ? body.messages : []
  const answered = messages.filter((m) => m.role === 'assistant').length
  return answered < script.length ? script[answered] : done
}

const server = createServer((req, res) => {
  if (!req.url?.endsWith('/chat/completions')) {
    res.writeHead(404).end('{}')
    return
  }
  let body = ''
  req.setEncoding('utf8')
  req.on('data', (chunk) => {
    body += chunk
  })
  req.on('end', () => {
    let parsed = null
    try {
      parsed = JSON.parse(body)
    } catch {
      /* an unreadable request gets the opening move, and the loop carries on */
    }
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(replyTo(parsed)))
  })
})

server.listen(port, '127.0.0.1', () => {
  process.stdout.write(`stub model listening on http://127.0.0.1:${port}/v1\n`)
})
