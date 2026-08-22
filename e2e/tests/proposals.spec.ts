import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { expect, test } from '@playwright/test'

const FIXTURES = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '../../testdata/dirty-retail',
)

/*
Accepting a proposed rule, in the browser, end to end.

This is the milestone in one file, and it is the same argument
TestAnAcceptedRuleIsEnforcedWithoutTheModel makes over HTTP: a model proposes an
expectation on one run, a person reads what it would permit and strikes out the
entry that is a mistake rather than a category, and the next audit finds the
defect with no model involved at all. The model is what converts a defect found
once into a check that runs forever; this screen is where a person decides it
does.

The striking out is not incidental. The fixture's status column holds "Actve"
alongside "Active", so a vocabulary materialized from the column permits the
typo by construction. Accepting it unread would enforce the misspelling rather
than catch it, which is exactly the outcome an accept screen that cannot show
its values would produce.
*/
test.describe.serial('accepting a rule the model proposed', () => {
  let datasetURL = ''
  let runURL = ''

  test('a run that used a model offers the rules it proposed', async ({ page }) => {
    await page.goto('/')
    await page.locator('input[type="file"]').setInputFiles(FIXTURES)
    await expect(page).toHaveURL(/\/datasets\/[0-9a-f-]+$/)
    datasetURL = page.url()

    await page.getByRole('checkbox', { name: /Also investigate with/ }).check()
    await page.getByRole('button', { name: 'Run an audit' }).click()
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    runURL = page.url()
    await expect(page.locator('.status')).toHaveText('succeeded')

    await page.getByRole('button', { name: /Rules proposed/ }).click()

    const proposal = page.locator('.proposal').first()
    await expect(proposal.locator('h4')).toHaveText(
      'customer status has to be one of the values in use today',
    )
    await expect(proposal.locator('.badge')).toHaveText('one_of')
    await expect(proposal.locator('.where')).toContainText('customers.csv.status')

    // A rule nothing breaks is the good case, and the screen says so rather
    // than reporting zero as if it were a failure to reproduce.
    await expect(proposal.locator('.num')).toContainText('Nothing breaks it today')
    await expect(proposal.locator('.num')).toContainText('5 values')

    // Nothing is in force until somebody says so.
    await expect(page.getByText('None is in force.')).toBeVisible()
  })

  test('the values are shown before anything is blessed, and can be struck out', async ({
    page,
  }) => {
    await page.goto(runURL)
    await page.getByRole('button', { name: /Rules proposed/ }).click()

    const proposal = page.locator('.proposal').first()
    await proposal.locator('.proposal-head').click()

    // The values are not on the page until they are asked for: this is the
    // second of the two endpoints that return raw cell values, and it is
    // reached one named proposal at a time.
    await expect(proposal.getByText('Actve', { exact: true })).toHaveCount(0)
    await proposal.getByRole('button', { name: 'Review the values and accept' }).click()

    const values = proposal.locator('.values')
    await expect(values.getByRole('checkbox', { name: 'Actve', exact: true })).toBeChecked()
    await expect(values.getByRole('checkbox', { name: 'Inactive', exact: true })).toBeChecked()
    await expect(values.locator('li')).toHaveCount(5)

    // The note above the list says what the list is: what the column held,
    // mistakes included.
    await expect(proposal.locator('.evidence-label')).toContainText(
      'not a vocabulary anybody chose',
    )

    await values.getByRole('checkbox', { name: 'Actve', exact: true }).uncheck()
    await expect(proposal.getByText(/1 struck out/)).toBeVisible()
    await expect(proposal.getByText(/reported from the next audit on/)).toBeVisible()

    // The reviewer confirms the severity rather than inheriting it silently.
    await expect(proposal.getByLabel('Report a violation as')).toHaveValue('error')

    await proposal.getByRole('button', { name: 'Accept this rule' }).click()
    await expect(proposal.getByText(/In force as/)).toBeVisible()
    await expect(proposal.locator('.where')).toContainText('in force')
  })

  test('the dataset lists what it now enforces on its own account', async ({ page }) => {
    await page.goto(datasetURL)

    const rules = page.locator('.rule-list li')
    await expect(rules).toHaveCount(1)
    await expect(rules.first()).toContainText('status_domain')
    // Written as the rule is written, which is the SQL name a rules file
    // targets rather than the source name the proposal screen showed.
    await expect(rules.first()).toContainText('customers_csv.status')

    // What is listed is the rule, not its contents: the permitted values are
    // the customer's data and a list screen never carries them.
    await expect(page.locator('.rule-list')).not.toContainText('Actve')
  })

  test('an audit with no model now finds what the model found once', async ({ page }) => {
    await page.goto(datasetURL)

    // No model this time: the checkbox is left alone, so this is the
    // deterministic auditor and nothing but it.
    await expect(page.getByRole('checkbox', { name: /Also investigate with/ })).not.toBeChecked()
    await page.getByRole('button', { name: 'Run an audit' }).click()
    await expect(page).toHaveURL(/\/runs\/[0-9a-f-]+$/)
    await expect(page.locator('.status')).toHaveText('succeeded')

    // No model ran, so there is no trace and nothing was proposed.
    await expect(page.locator('.agent-banner')).toHaveCount(0)
    await expect(page.getByRole('button', { name: /Rules proposed/ })).toHaveCount(0)

    const finding = page.locator('.finding', { hasText: 'status_domain' }).first()
    await expect(finding.locator('h4')).toContainText('1 row(s) break rule "status_domain"')
    await expect(finding.locator('.where')).toContainText('customers.csv.status')
    await expect(finding.locator('.where')).toContainText('your rule')
  })

  test('the same rule is not offered for accepting twice', async ({ page }) => {
    await page.goto(runURL)
    await page.getByRole('button', { name: /Rules proposed/ }).click()

    const proposal = page.locator('.proposal').first()
    await proposal.locator('.proposal-head').click()

    // A proposal's id is what the rule asserts rather than how it was worded,
    // so the rule accepted above — renamed or not — is recognized here on a
    // fresh page load rather than being offered again and refused on press.
    await expect(proposal.getByText(/already in force for this dataset/)).toBeVisible()
    await expect(
      proposal.getByRole('button', { name: 'Review the values and accept' }),
    ).toHaveCount(0)
  })
})
