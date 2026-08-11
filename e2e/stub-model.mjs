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

let call = 0

const server = createServer((req, res) => {
  if (!req.url?.endsWith('/chat/completions')) {
    res.writeHead(404).end('{}')
    return
  }
  // The body is drained rather than read: what was in it is asserted by the Go
  // tests, which can scan it against the fixture's contents.
  req.resume()
  req.on('end', () => {
    const reply = call < script.length ? script[call] : done
    call += 1
    res.writeHead(200, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(reply))
  })
})

server.listen(port, '127.0.0.1', () => {
  process.stdout.write(`stub model listening on http://127.0.0.1:${port}/v1\n`)
})
