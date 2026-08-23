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
Auditing on a schedule, in the browser.

Like the comparison spec this one owns a directory and registers it by path
rather than uploading. That is not incidental: an upload is a copy of the data
as it was and never changes again, so the server refuses to schedule one, and
the panel is not offered for it.
*/
test.describe.serial('audit on a schedule', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'veritix-schedule-'))
  let datasetURL = ''

  test.beforeAll(async ({ request }) => {
    for (const name of ['customers.csv', 'orders.csv', 'regions.csv']) {
      fs.copyFileSync(path.join(FIXTURES, name), path.join(dir, name))
    }
    const res = await request.post('/api/v1/datasets', { data: { path: dir } })
    expect(res.status()).toBe(201)
    const ds = (await res.json()) as { id: string }
    datasetURL = `/datasets/${ds.id}`
  })

  test.afterAll(() => {
    fs.rmSync(dir, { recursive: true, force: true })
  })

  test('a dataset can be told to audit itself every night', async ({ page }) => {
    await page.goto(datasetURL)

    await expect(page.getByRole('heading', { name: 'Audit on a schedule' })).toBeVisible()
    // Nothing is scheduled until somebody says so.
    await expect(page.getByTestId('schedule-state')).toHaveCount(0)

    await page.getByLabel('How often').selectOption('daily')
    await page.getByLabel('Time of day').fill('02:00')
    await page.getByLabel('Time zone').fill('Europe/London')
    await page.getByRole('button', { name: 'Save schedule' }).click()

    // The window it is waiting for comes back from the server, so the screen
    // is showing what was stored rather than what was typed.
    await expect(page.getByTestId('schedule-state')).toContainText('Next audit')
  })

  test('the schedule is still there after a reload, and can be turned off', async ({
    page,
  }) => {
    await page.goto(datasetURL)

    await expect(page.getByLabel('How often')).toHaveValue('daily')
    await expect(page.getByLabel('Time of day')).toHaveValue('02:00')
    await expect(page.getByLabel('Time zone')).toHaveValue('Europe/London')
    await expect(page.getByTestId('schedule-state')).toContainText('Next audit')

    await page.getByLabel('How often').selectOption('off')
    await page.getByRole('button', { name: 'Turn off' }).click()
    await expect(page.getByTestId('schedule-state')).toHaveCount(0)

    await page.reload()
    await expect(page.getByLabel('How often')).toHaveValue('off')
  })

  test('a schedule audits the dataset with nobody pressing anything', async ({
    page,
    request,
  }) => {
    // A minute is the shortest gap Veritix will accept, and it is the product's
    // own floor rather than a test convenience — so this test waits for one.
    test.setTimeout(180_000)

    const res = await request.put(`/api/v1${datasetURL}/schedule`, {
      data: { kind: 'interval', every_minutes: 1 },
    })
    expect(res.status()).toBe(200)

    await expect
      .poll(
        async () => {
          await page.goto(datasetURL)
          return page.locator('table tbody tr').count()
        },
        {
          message: 'the clock never audited the dataset',
          timeout: 150_000,
          intervals: [5_000],
        },
      )
      .toBeGreaterThan(0)

    // It is an ordinary run: in the history, and it opens like any other, with
    // the report there to download.
    await expect(page.locator('.status').first()).toHaveText('succeeded')
    await page.locator('table tbody tr a').first().click()
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    await expect(page.getByRole('heading', { name: 'Audit' })).toBeVisible()
    await expect(page.getByRole('link', { name: 'Download the report' })).toBeVisible()
  })
})
