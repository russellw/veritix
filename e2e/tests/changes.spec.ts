import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test } from '@playwright/test'

const FIXTURES = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../testdata/dirty-retail',
)

/*
What changed since the last audit, in the browser.

Every other spec uploads a fixture, which is what a business user does. This one
cannot: an upload lands in a new directory and so registers a new dataset, and
two runs of two datasets have no history between them. The comparison is about
the same folder audited twice with the data moving underneath, so the test owns
a directory on disk, registers it by path, and edits it between runs.

Registering by path is what an operator does with data already on the server —
the API takes it, and the two dataset screens are identical afterwards.
*/
test.describe.serial('what changed since the last audit', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'veritix-changes-'))
  const csvs = ['customers.csv', 'orders.csv', 'regions.csv']
  let datasetURL = ''

  test.beforeAll(() => {
    for (const name of csvs) {
      fs.copyFileSync(path.join(FIXTURES, name), path.join(dir, name))
    }
  })

  test.afterAll(() => {
    fs.rmSync(dir, { recursive: true, force: true })
  })

  test('the first audit of a dataset has nothing to compare against', async ({ page }) => {
    const res = await page.request.post('/api/v1/datasets', { data: { path: dir } })
    expect(res.status()).toBe(201)
    const ds = (await res.json()) as { id: string }
    datasetURL = `/datasets/${ds.id}`

    await page.goto(datasetURL)
    await page.getByRole('button', { name: 'Run an audit' }).click()
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    await expect(page.locator('.status')).toHaveText('succeeded')

    // Nothing to compare with, so neither half of the comparison appears.
    await expect(page.locator('.change-strip')).toHaveCount(0)
    await expect(page.getByRole('button', { name: /^Changes/ })).toHaveCount(0)
  })

  test('the second audit says what moved, and what stayed the same', async ({ page }) => {
    // One more order pointing at a customer who does not exist: an existing
    // finding gets worse rather than a new one appearing, which is the case a
    // list of findings cannot show on its own.
    fs.appendFileSync(
      path.join(dir, 'orders.csv'),
      '9999,CUS-999999,2024-03-01,10.00,GBP\n',
    )

    await page.goto(datasetURL)
    await page.getByRole('button', { name: 'Run an audit' }).click()
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    await expect(page.locator('.status')).toHaveText('succeeded')

    // The strip is under the counts, where somebody reads it without having
    // decided to.
    const strip = page.locator('.change-strip')
    await expect(strip).toBeVisible()
    await expect(strip).toContainText('Since the previous audit')
    await expect(strip.locator('.worse')).toContainText('worse')

    await strip.getByRole('button', { name: 'what changed' }).click()

    const worsened = page.locator('.change.worsened').first()
    await expect(worsened.locator('.badge')).toHaveText('worsened')
    await expect(worsened.locator('.where')).toContainText('reference.orphan_values')
    await expect(worsened.locator('.where')).toContainText('→')

    // The table gained a row, and this is the only place in the product that
    // can say so: no check that reads one audit can see it.
    const table = page.locator('.changes .change', { hasText: 'orders.csv' }).last()
    await expect(table.locator('.where')).toContainText('rows')
  })

  test('a finding that moved opens in the findings list', async ({ page }) => {
    await page.goto(datasetURL)

    // Into the most recent run from the history table rather than from a
    // remembered URL, so this reaches the tab the way a reader does. The list
    // is newest first.
    await page.locator('tbody tr a').first().click()
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    await expect(page.locator('.status')).toHaveText('succeeded')

    await page.getByRole('button', { name: /^Changes/ }).click()
    await page.locator('.change.worsened .title').first().click()

    // The link changes the tab as well as the address bar: a URL that showed
    // nothing would be worse than no link.
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+\/findings\/[0-9a-f]+$/)
    await expect(page.locator('.finding.target')).toBeVisible()
    await expect(page.locator('.finding.target')).toContainText('no matching row')
  })
})
