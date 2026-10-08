// 📌 影响范围：拦截违禁监控专属 API 并维护内存测试状态；不访问真实数据库、NapCat 或外部模型。
import { expect, test, type Page, type Route } from '@playwright/test'

async function fulfill(route: Route, data: unknown): Promise<void> {
  await route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({ code: 'ok', message: '成功', data }),
  })
}

async function seedSession(page: Page): Promise<void> {
  await page.addInitScript(() => sessionStorage.setItem('w1ndys_bot_token', 'e2e-token'))
}

interface MockOptions {
  termCreateConflict?: boolean
  termCreateFailure?: boolean
  termCreateDelayMs?: number
  failInitialTermLoads?: number
  failTermRefreshAfterCreate?: boolean
}

interface MockState {
  termCreateCalls: number
}

async function mockForbiddenMonitor(page: Page, options: MockOptions = {}): Promise<MockState> {
  let terms: Array<Record<string, unknown>> = []
  let failedTermLoads = 0
  let failNextTermLoad = false
  const state: MockState = { termCreateCalls: 0 }
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    expect(request.headers().authorization).toBe('Bearer e2e-token')
    const url = new URL(request.url())
    const key = `${request.method()} ${url.pathname}`
    switch (key) {
      case 'GET /api/plugins/forbidden_message_monitor/violations':
        await fulfill(route, { items: [], page: 1, page_size: 20, total: 0 })
        return
      case 'GET /api/plugins/forbidden_message_monitor/terms':
        expect(url.searchParams.has('kind')).toBe(false)
        if (failedTermLoads < (options.failInitialTermLoads ?? 0) || failNextTermLoad) {
          failedTermLoads++
          failNextTermLoad = false
          await route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({ code: 'temporarily_unavailable', message: '列表暂时不可用', data: null }),
          })
          return
        }
        await fulfill(route, { items: terms, page: 1, page_size: 20, total: terms.length })
        return
      case 'POST /api/plugins/forbidden_message_monitor/text-trials': {
        expect(request.postDataJSON()).toEqual({ text: '加 微信，领取资料' })
        await fulfill(route, {
          id: 17,
          version: 1,
          data: {
            decision: '违规',
            stage: 'llm',
            cleaned_text: '加微信领取资料',
            cleaned_length: 7,
            local_score: 25,
            matched_terms: ['加微信'],
            reason: '模型判断为引流广告',
            llm_used: true,
            llm_risk_level: 'High',
            llm_total_score: 92,
            suggested_action: 'block',
          },
        })
        return
      }
      case 'POST /api/plugins/forbidden_message_monitor/terms': {
        state.termCreateCalls++
        expect(request.postDataJSON()).toEqual({ text: '加微信', weight: 25 })
        if (options.termCreateConflict) {
          await route.fulfill({
            status: 409,
            contentType: 'application/json',
            body: JSON.stringify({ code: 'forbidden_monitor_conflict', message: '违禁词已被其他操作更新', data: null }),
          })
          return
        }
        if (options.termCreateFailure) {
          await route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({ code: 'temporarily_unavailable', message: '数据库暂时不可用', data: null }),
          })
          return
        }
        if (options.termCreateDelayMs) await new Promise((resolve) => setTimeout(resolve, options.termCreateDelayMs))
        terms = [{ id: 5, text: '加微信', weight: 25, version: 1, updated_at: '2026-07-26T12:00:00Z' }]
        failNextTermLoad = options.failTermRefreshAfterCreate === true
        await fulfill(route, terms[0])
        return
      }
      default:
        throw new Error(`未处理违禁监控 API：${key}`)
    }
  })
  return state
}

test('违禁监控页面只保留复核、试判和加权违禁词', async ({ page }) => {
  await seedSession(page)
  await mockForbiddenMonitor(page)
  await page.goto('/plugin-pages/forbidden_message_monitor')

  await expect(page.getByText('当前没有待复核记录')).toBeVisible()
  await expect(page.locator('.n-tabs-tab')).toHaveCount(3)
  await expect(page.getByText('违规复核', { exact: true })).toBeVisible()
  await expect(page.getByText('文本试判', { exact: true })).toBeVisible()
  await expect(page.getByText('违禁词', { exact: true })).toBeVisible()
  await expect(page.getByText('训练样本', { exact: true })).toHaveCount(0)
  await expect(page.getByText('组合规则', { exact: true })).toHaveCount(0)

  await page.getByText('文本试判', { exact: true }).click()
  await page.locator('textarea').fill('加 微信，领取资料')
  await page.getByRole('button', { name: '开始试判' }).click()
  await expect(page.getByText('文本试判完成')).toBeVisible()
  await expect(page.getByText('加微信领取资料')).toBeVisible()
  await expect(page.getByText('7', { exact: true })).toBeVisible()
  await expect(page.getByText('25', { exact: true })).toBeVisible()
  await expect(page.getByText('加微信', { exact: true })).toBeVisible()
  await expect(page.getByText('High / 92')).toBeVisible()
  await expect(page.getByText('模型判断为引流广告')).toBeVisible()

  await page.getByText('违禁词', { exact: true }).click()
  await expect(page.getByText('暂无违禁词')).toBeVisible()
  await page.getByRole('button', { name: '新增违禁词' }).click()
  await expect(page.getByText('硬拦截', { exact: true })).toHaveCount(0)
  await expect(page.getByText('安全抵扣', { exact: true })).toHaveCount(0)
  await page.getByPlaceholder('违禁词文本').fill('加微信')
  await page.locator('.n-input-number input').fill('25')
  await page.getByRole('button', { name: '保存', exact: true }).click()
  await expect(page.getByText('词条已新增')).toBeVisible()
  await expect(page.getByRole('cell', { name: '加微信' })).toBeVisible()
})

test('词条冲突使用全局反馈、刷新数据并保留编辑现场', async ({ page }) => {
  await seedSession(page)
  await mockForbiddenMonitor(page, { termCreateConflict: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto('/plugin-pages/forbidden_message_monitor')

  await page.getByText('违禁词', { exact: true }).click()
  await page.getByRole('button', { name: '新增违禁词' }).click()
  await page.getByPlaceholder('违禁词文本').fill('加微信')
  await page.locator('.n-input-number input').fill('25')
  await page.getByRole('button', { name: '保存', exact: true }).click()

  await expect(page.getByText('违禁词已被其他操作更新，数据可能已变化，请刷新后重试')).toBeVisible()
  await expect(page.getByRole('heading', { name: '新增违禁词', exact: true })).toBeVisible()
  await expect(page.getByPlaceholder('违禁词文本')).toHaveValue('加微信')
  await expect(page.locator('body')).toHaveJSProperty('scrollWidth', 390)
})

test('初始加载失败可在页面内重试', async ({ page }) => {
  await seedSession(page)
  await mockForbiddenMonitor(page, { failInitialTermLoads: 1 })
  await page.goto('/plugin-pages/forbidden_message_monitor')

  await expect(page.getByText('数据加载失败')).toBeVisible()
  await expect(page.getByText('列表暂时不可用')).toBeVisible()
  await page.getByRole('button', { name: '重试' }).click()
  await expect(page.getByText('数据加载失败')).toHaveCount(0)
  await expect(page.getByText('当前没有待复核记录')).toBeVisible()
})

test('词条写入成功但刷新失败时关闭表单并提示重试', async ({ page }) => {
  await seedSession(page)
  await mockForbiddenMonitor(page, { failTermRefreshAfterCreate: true })
  await page.goto('/plugin-pages/forbidden_message_monitor')

  await page.getByText('违禁词', { exact: true }).click()
  await page.getByRole('button', { name: '新增违禁词' }).click()
  await page.getByPlaceholder('违禁词文本').fill('加微信')
  await page.locator('.n-input-number input').fill('25')
  await page.getByRole('button', { name: '保存', exact: true }).click()

  await expect(page.getByText('词条已新增，但数据刷新失败，请点击重试')).toBeVisible()
  await expect(page.getByRole('heading', { name: '新增违禁词', exact: true })).toHaveCount(0)
  await page.getByRole('button', { name: '重试' }).click()
  await expect(page.getByRole('cell', { name: '加微信' })).toBeVisible()
})

test('非冲突错误不显示数据变化提示', async ({ page }) => {
  await seedSession(page)
  await mockForbiddenMonitor(page, { termCreateFailure: true })
  await page.goto('/plugin-pages/forbidden_message_monitor')

  await page.getByText('违禁词', { exact: true }).click()
  await page.getByRole('button', { name: '新增违禁词' }).click()
  await page.getByPlaceholder('违禁词文本').fill('加微信')
  await page.locator('.n-input-number input').fill('25')
  await page.getByRole('button', { name: '保存', exact: true }).click()

  await expect(page.getByText('数据库暂时不可用', { exact: true })).toBeVisible()
  await expect(page.getByText(/数据可能已变化/)).toHaveCount(0)
})

test('词条保存期间禁用重复提交', async ({ page }) => {
  await seedSession(page)
  const state = await mockForbiddenMonitor(page, { termCreateDelayMs: 300 })
  await page.goto('/plugin-pages/forbidden_message_monitor')

  await page.getByText('违禁词', { exact: true }).click()
  await page.getByRole('button', { name: '新增违禁词' }).click()
  await page.getByPlaceholder('违禁词文本').fill('加微信')
  await page.locator('.n-input-number input').fill('25')
  const save = page.getByRole('button', { name: '保存', exact: true })
  await save.click()
  await expect(save).toBeDisabled()
  await expect(page.getByText('词条已新增')).toBeVisible()
  expect(state.termCreateCalls).toBe(1)
})
