import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test } from '@playwright/test'

const FIXTURES = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../testdata/dirty-retail',
)

/*
The agentic screens, driven against the stub model that run-local.sh starts.

What these prove that no Go test can: that a user is told a model was involved
before they read what it found, that the trace screen actually renders the
payloads, and — the one that matters most — that what a customer sees on that
screen when they go looking for their data is shapes rather than their data.
The Go tests assert the same thing about the bytes on the wire; this asserts it
about the thing a person actually reads.
*/
test.describe.serial('investigating with a model', () => {
  let runURL = ''

  test('a model investigates alongside the checks, and says so', async ({ page }) => {
    await page.goto('/')
    await page.locator('input[type="file"]').setInputFiles(FIXTURES)
    await expect(page).toHaveURL(/\/datasets\/[0-9a-f-]+$/)

    // The option is offered because a model is configured. On a server without
    // one it is absent rather than present and broken.
    const investigate = page.getByRole('checkbox', { name: /Also investigate with/ })
    await expect(investigate).toBeVisible()
    await investigate.check()

    // Ticking it explains what will be sent before anything is sent.
    await expect(page.getByText(/column names, counts, distributions/)).toBeVisible()
    await expect(
      page.getByRole('checkbox', { name: /Let it see cell values too/ }),
    ).not.toBeChecked()

    await page.getByRole('button', { name: 'Run an audit' }).click()
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    runURL = page.url()

    await expect(page.locator('.status')).toHaveText('succeeded')

    // The banner sits above the findings, not after them.
    const banner = page.locator('.agent-banner')
    await expect(banner).toBeVisible()
    await expect(banner).toContainText('stub-model')
    await expect(banner).toContainText('No cell value was sent to it')
    // The stub's first attempt claimed 400 rows against a query returning 1.
    await expect(banner).toContainText('did not reproduce')
  })

  test('a claim the engine contradicts never reaches the report', async ({ page }) => {
    await page.goto(runURL)

    // The stub first claimed 400 rows against a query returning 1. That
    // attempt was refused, so the discredited figure appears nowhere — not in
    // the finding's count, and not in the title either, which is the part a
    // silent correction would have left wrong.
    await expect(page.getByText('400 orders')).toHaveCount(0)

    const finding = page.locator('.finding', { hasText: 'negative amount' }).first()
    await expect(finding).toBeVisible()
    await expect(finding.locator('h4')).toHaveText('1 order is recorded with a negative amount')
    await expect(finding.locator('.where')).toContainText('proposed by the model, verified')

    await finding.locator('.finding-head').click()
    await expect(finding.locator('.num')).toContainText('1 of 8 rows')
    await expect(finding.locator('pre')).toContainText('TRY_CAST(amount AS DOUBLE) < 0')
  })

  test('the trace shows what was sent, and it is shapes rather than data', async ({ page }) => {
    await page.goto(runURL)

    await page.getByRole('button', { name: /What the model saw/ }).click()

    const trace = page.locator('.trace')
    await expect(trace).toBeVisible()
    await expect(trace).toContainText('No cell value was sent to the model')

    // Open the describe_table call: this is the payload with the most of the
    // customer's data in it, so it is the one worth looking at.
    const call = trace.locator('.call', { hasText: 'describe_table' }).first()
    await call.locator('.call-head').click()

    const payload = call.locator('pre').nth(1)
    await expect(payload).toContainText('shapes')
    // Shapes of the fixture's customer references, not the references.
    await expect(payload).toContainText('XXX-999999')
    await expect(payload).not.toContainText('CUS-000001')
    await expect(payload).not.toContainText('alice@example.com')
  })
})
