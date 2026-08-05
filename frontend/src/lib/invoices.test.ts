import { describe, expect, it } from 'vitest'

import {
  trustworthyNetIssuedMinor,
  type InvoiceSummaryGroup,
  type InvoiceSummarySourceHealth,
} from './invoices'

const baseGroup: InvoiceSummaryGroup = {
  currency: 'CNY',
  minor_unit_scale: 2,
  blue_issued_minor: '100',
  red_issued_minor: '25',
  voided_blue_minor: '0',
  voided_red_minor: '0',
  voided_minor: '0',
  net_issued_minor: '75',
  voided_count: 0,
  effective_count: 2,
  source_health: 'ok',
  unreconciled_count: 0,
  anomaly_count: 0,
}

const reconciledOverall = { status: 'ok', unreconciled_count: 0, anomaly_count: 0 }

describe('invoice summary truth contract', () => {
  it('shows net issued amount only when group and response are reconciled', () => {
    expect(trustworthyNetIssuedMinor(baseGroup, reconciledOverall)).toBe('75')
    expect(trustworthyNetIssuedMinor({ ...baseGroup, source_health: 'unreconciled' }, reconciledOverall)).toBeNull()
    expect(trustworthyNetIssuedMinor(baseGroup, { ...reconciledOverall, status: 'unreconciled' })).toBeNull()
    expect(trustworthyNetIssuedMinor({ ...baseGroup, net_issued_minor: null }, reconciledOverall)).toBeNull()
  })

  it.each([
    ['group unreconciled', { ...baseGroup, unreconciled_count: 1 }, reconciledOverall],
    ['overall unreconciled', baseGroup, { ...reconciledOverall, unreconciled_count: 1 }],
    ['group anomaly', { ...baseGroup, anomaly_count: 1 }, reconciledOverall],
    ['overall anomaly', baseGroup, { ...reconciledOverall, anomaly_count: 1 }],
    ['missing group count', { ...baseGroup, unreconciled_count: undefined }, reconciledOverall],
    ['missing overall count', baseGroup, { status: 'ok' }],
    ['missing group anomaly count', { ...baseGroup, anomaly_count: undefined }, reconciledOverall],
    ['missing overall anomaly count', baseGroup, { ...reconciledOverall, anomaly_count: undefined }],
    ['fractional group count', { ...baseGroup, unreconciled_count: 0.5 }, reconciledOverall],
    ['negative overall count', baseGroup, { ...reconciledOverall, unreconciled_count: -1 }],
    ['fractional group anomaly count', { ...baseGroup, anomaly_count: 0.5 }, reconciledOverall],
    ['negative overall anomaly count', baseGroup, { ...reconciledOverall, anomaly_count: -1 }],
  ])('fails closed for contradictory or malformed counts: %s', (_name, group, overall) => {
    expect(trustworthyNetIssuedMinor(
      group as InvoiceSummaryGroup,
      overall as InvoiceSummarySourceHealth,
    )).toBeNull()
  })
})
