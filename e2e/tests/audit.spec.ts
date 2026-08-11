import { readFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test } from '@playwright/test'

// Kept in step with playwright.config.ts, which reads the same variable.
const BASE_URL = process.env.BASE_URL ?? 'http://localhost:8080'

// The fixtures with the known defect manifest. Uploading the directory is what
// a business user actually does, and it exercises the browser's folder upload —
// relative paths in filenames, which the server reduces to base names.
const FIXTURES = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../testdata/dirty-retail',
)

/*
These tests run against the Go binary serving the embedded build — the thing
that ships — rather than the Vite dev server. What they are here to prove is the
part no Go test can reach: that the bundle actually loads under the strict CSP,
that SSE progress arrives and drives a re-render, and that the one screen which
reveals raw customer data does so only when asked.
*/
test.describe.serial('auditing a folder from the browser', () => {
  // Shared because the journey is one story told in order, and each step is
  // worth failing separately. serial mode stops the rest once one breaks.
  let runURL = ''
  let findingURL = ''

  test('a folder of exports is uploaded and audited', async ({ page }) => {
    await page.goto('/')
    await expect(page.getByRole('heading', { name: 'Audit a dataset' })).toBeVisible()

    // The picker is hidden behind the drop zone; a file input takes files
    // whether or not it is visible, and a real drag-and-drop is not something
    // Playwright can synthesise for the filesystem entries API.
    await page.locator('input[type="file"]').setInputFiles(FIXTURES)

    // The upload lands on the dataset, named after the folder.
    await expect(page).toHaveURL(/\/datasets\/[0-9a-f-]+$/)
    await expect(page.getByRole('heading', { name: 'dirty-retail' })).toBeVisible()
    await expect(page.getByText('Uploaded to this server')).toBeVisible()

    await page.getByRole('button', { name: 'Run an audit' }).click()

    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    runURL = page.url()

    // Nothing is clicked or reloaded between starting the run and the findings
    // appearing: if they show up, the SSE stream delivered the terminal event
    // and the app fetched the report off the back of it.
    await expect(page.locator('.status')).toHaveText('succeeded')
    await expect(page.getByRole('button', { name: /^Findings \(\d+\)/ })).toBeVisible()

    // The fixtures are deliberately broken, so a run that finds nothing means
    // the pipeline did not really execute.
    const errors = page.locator('.tile.error .n').first()
    await expect(errors).not.toHaveText('0')
  })

  test('a finding opens to its evidence, and its rows stay hidden until asked for', async ({
    page,
  }) => {
    await page.goto(runURL)

    const finding = page.locator('.finding').first()
    await expect(finding).toBeVisible()

    // Collapsed: the explanation and the evidence are not in the page at all,
    // not merely hidden with CSS.
    await expect(finding.locator('pre')).toHaveCount(0)

    await finding.locator('.finding-head').click()

    // The evidence query is the claim's receipt — a reader has to be able to
    // check the number rather than take it on trust.
    await expect(finding.locator('pre')).toBeVisible()
    await expect(finding.locator('.evidence-label')).toContainText('rather than take it on trust')

    // Expanding puts the finding in the address bar, because a finding id is
    // stable across runs and the link is worth sending to somebody.
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+\/findings\/[0-9a-f]+$/)
    findingURL = page.url()

    // The gate: no customer data on screen until it is asked for by name.
    const reveal = finding.getByRole('button', { name: 'Show the offending rows' })
    await expect(reveal).toBeVisible()
    await expect(finding.locator('table')).toHaveCount(0)

    await reveal.click()

    const rows = finding.locator('table')
    await expect(rows).toBeVisible()
    await expect(rows.locator('tbody tr')).not.toHaveCount(0)

    // And it can be put away again.
    await finding.getByRole('button', { name: 'Hide' }).click()
    await expect(finding.locator('table')).toHaveCount(0)
  })

  test('a link to a finding survives a reload', async ({ page }) => {
    // The server has no such route: this only works because the SPA handler
    // falls back to index.html and the app reads the path back out.
    await page.goto(findingURL)

    const target = page.locator('.finding.target')
    await expect(target).toBeVisible()
    await expect(target.locator('pre')).toBeVisible()
  })

  test('the profile is a second tab, not the landing view', async ({ page }) => {
    await page.goto(runURL)

    // Findings first. A profile of a clean dataset is a wall of unremarkable
    // numbers, and burying real problems inside it is how a tool gets ignored.
    await expect(page.locator('.finding').first()).toBeVisible()

    await page.getByRole('button', { name: /^Tables \(\d+\)/ }).click()
    await expect(page.getByRole('columnheader', { name: 'Column' }).first()).toBeVisible()
    await expect(page.locator('.finding')).toHaveCount(0)
  })

  test('the report downloads as one self-contained file', async ({ page }) => {
    await page.goto(runURL)

    const download = page.waitForEvent('download')
    await page.getByRole('link', { name: 'Download the report' }).click()
    const file = await download

    expect(file.suggestedFilename()).toMatch(/\.html$/)

    const saved = await file.path()
    const body = await readFile(saved, 'utf8')
    expect(body).toContain('<!doctype html>')
    // The report is emailed and opened on a laptop with no network. A report
    // that fetches anything to render is also one that tells somebody else
    // which datasets are being audited.
    expect(body).not.toMatch(/(src|href)\s*=\s*["'](https?:)?\/\//i)
  })
})

test('the page talks to its own server and nowhere else', async ({ page }) => {
  const origin = new URL(BASE_URL).origin
  const foreign: string[] = []

  page.on('request', (req) => {
    if (!req.url().startsWith(origin) && !req.url().startsWith('data:')) {
      foreign.push(req.url())
    }
  })

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Audit a dataset' })).toBeVisible()

  // The CSP's connect-src 'self' is what enforces this; the assertion is here
  // so that a reference which the CSP would silently block is caught as a test
  // failure rather than as a page that half-works.
  expect(foreign, `the page requested ${foreign.join(', ')}`).toHaveLength(0)
})

test('the interface offers the source of what it is running', async ({ page }) => {
  await page.goto('/')

  // AGPL section 13: a modified Veritix served over a network owes its users
  // the source. The footer is where that offer lives, and the URL comes from
  // the server so that an operator running a fork can point it at their own
  // repository without rebuilding this bundle.
  const colophon = page.locator('.colophon')
  await expect(colophon).toContainText('Veritix')
  await expect(colophon.getByRole('link', { name: 'Source' })).toHaveAttribute(
    'href',
    /^https?:\/\//,
  )
})

test('a server-side folder can be registered without uploading it', async ({ page }) => {
  await page.goto('/')

  // The operator path: the data is already on the server, so registering it
  // reads it in place rather than copying it into the data directory.
  await page.getByRole('button', { name: 'or read a folder already on the server' }).click()
  await page.getByPlaceholder('/srv/exports/retail').fill(FIXTURES)
  await page.getByRole('button', { name: 'Read it' }).click()

  await expect(page).toHaveURL(/\/datasets\/[0-9a-f-]+$/)
  await expect(page.getByText('Read in place from this server')).toBeVisible()
})
