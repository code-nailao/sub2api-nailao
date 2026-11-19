import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { createPinia } from 'pinia'
import AdminOrdersView from '../AdminOrdersView.vue'
const api = vi.hoisted(() => ({ getOrders: vi.fn(), retryRecharge: vi.fn(), reconcileUSDT: vi.fn() }))
vi.mock('@/api/admin/payment', () => ({ adminPaymentAPI: api, default: api }))
vi.mock('vue-i18n', async (original) => ({ ...await original<object>(), useI18n: () => ({ t: (key: string) => key }) }))
const review = { id: 1, user_id: 1, amount: 100, pay_amount: 100, fee_rate: 0, status: 'PAYMENT_REVIEW', payment_type: 'usdt_bsc', provider_key: 'bepusdt', payment_trade_no: '', out_trade_no: 'MAIN_USDT_fixture', order_type: 'balance', refund_amount: 0, created_at: '', expires_at: '' }
function screen() { return mount(AdminOrdersView, { global: { plugins: [createPinia()], stubs: { AppLayout: { template: '<main><slot /></main>' }, OrderTable: { props: ['orders'], template: '<div><div v-for="row in orders" :key="row.id"><slot name="actions" :row="row" /></div></div>' }, BaseDialog: true, Select: true, Pagination: true, RefundDialog: true, OrderStatusBadge: true, Icon: true } } }) }
beforeEach(() => { api.getOrders.mockResolvedValue({ data: { items: [review], total: 1 } }); api.retryRecharge.mockReset().mockResolvedValue({}); api.reconcileUSDT.mockReset().mockResolvedValue({}) })
afterEach(() => vi.restoreAllMocks())
describe('payment recovery actions', () => {
 it('requires explicit review approval before calling the existing retry endpoint', async () => {
  api.getOrders.mockResolvedValue({ data: { items: [{ ...review, payment_trade_no: 'verified-original' }], total: 1 } });const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);const w = screen();await flushPromises()
  const button = w.findAll('button').find(b => b.text() === 'payment.admin.reviewPayment')!;expect(button).toBeTruthy();await button.trigger('click');expect(api.retryRecharge).not.toHaveBeenCalled()
  confirm.mockReturnValue(true);await button.trigger('click');await flushPromises();expect(api.retryRecharge).toHaveBeenCalledWith(1);w.unmount()
 })
 it('submits only the original order ID and gateway reference, not a credit amount', async () => {
  vi.spyOn(window, 'prompt').mockReturnValue('gateway_fixture');const w = screen();await flushPromises();const button=w.findAll('button').find(b=>b.text()==='payment.admin.reconcileTrade')!;await button.trigger('click');await flushPromises();expect(api.reconcileUSDT).toHaveBeenCalledWith(1,'gateway_fixture');expect(api.retryRecharge).not.toHaveBeenCalled();w.unmount()
 })
})
