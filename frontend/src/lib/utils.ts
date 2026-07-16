import { type ClassValue, clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// Cloudflare IP ranges
// https://www.cloudflare.com/ips/
const CLOUDFLARE_IPV4_RANGES = [
  '173.245.48.0/20',
  '103.21.244.0/22',
  '103.22.200.0/22',
  '103.31.4.0/22',
  '141.101.64.0/18',
  '108.162.192.0/18',
  '190.93.240.0/20',
  '188.114.96.0/20',
  '197.234.240.0/22',
  '198.41.128.0/17',
  '162.158.0.0/15',
  '104.16.0.0/13',
  '104.24.0.0/14',
  '172.64.0.0/13',
  '131.0.72.0/22',
]

const CLOUDFLARE_IPV6_RANGES = [
  '2400:cb00::/32',
  '2606:4700::/32',
  '2803:f800::/32',
  '2405:b500::/32',
  '2405:8100::/32',
  '2a06:98c0::/29',
  '2c0f:f248::/32',
]

const IPV4_OCTET_PATTERN = /^(0|[1-9]\d{0,2})$/
const IPV6_GROUP_PATTERN = /^[0-9a-f]{1,4}$/i
const CIDR_PREFIX_PATTERN = /^(0|[1-9]\d*)$/

/** Parse a canonical, unambiguous IPv4 address into an unsigned 32-bit value. */
function parseIpv4(ip: string): number | null {
  const parts = ip.split('.')
  if (parts.length !== 4 || parts.some((part) => !IPV4_OCTET_PATTERN.test(part))) {
    return null
  }
  const octets = parts.map(Number)
  if (octets.some((octet) => octet > 255)) return null
  return (
    (octets[0] * 0x1000000)
    + (octets[1] * 0x10000)
    + (octets[2] * 0x100)
    + octets[3]
  ) >>> 0
}

/**
 * Check if an IPv4 address is within a CIDR range
 */
function isIpv4InCidr(ip: string, cidr: string): boolean {
  const cidrParts = cidr.split('/')
  if (cidrParts.length !== 2 || !CIDR_PREFIX_PATTERN.test(cidrParts[1])) return false
  const prefix = Number(cidrParts[1])
  if (!Number.isInteger(prefix) || prefix < 0 || prefix > 32) return false
  const ipNum = parseIpv4(ip)
  const rangeNum = parseIpv4(cidrParts[0])
  if (ipNum === null || rangeNum === null) return false
  const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0
  return (ipNum & mask) === (rangeNum & mask)
}

/** Parse IPv6, including a final embedded IPv4 address, into a 128-bit value. */
function parseIpv6(ip: string): bigint | null {
  if (!ip || ip.includes('%') || ip.includes('/') || ip !== ip.trim()) return null

  let normalized = ip
  if (normalized.includes('.')) {
    const lastColon = normalized.lastIndexOf(':')
    if (lastColon < 0) return null
    const ipv4 = parseIpv4(normalized.slice(lastColon + 1))
    if (ipv4 === null) return null
    normalized = `${normalized.slice(0, lastColon)}:${(ipv4 >>> 16).toString(16)}:${(ipv4 & 0xffff).toString(16)}`
  }

  const compressionIndex = normalized.indexOf('::')
  if (compressionIndex !== normalized.lastIndexOf('::')) return null
  const compressed = compressionIndex >= 0
  const sides = compressed ? normalized.split('::') : [normalized]
  if (sides.length > 2) return null

  const left = sides[0] ? sides[0].split(':') : []
  const right = compressed && sides[1] ? sides[1].split(':') : []
  if ([...left, ...right].some((group) => !IPV6_GROUP_PATTERN.test(group))) return null

  const explicitGroupCount = left.length + right.length
  if ((!compressed && explicitGroupCount !== 8) || (compressed && explicitGroupCount >= 8)) {
    return null
  }

  const groups = compressed
    ? [...left, ...Array<string>(8 - explicitGroupCount).fill('0'), ...right]
    : left
  if (groups.length !== 8) return null

  let result = BigInt(0)
  for (const group of groups) {
    result = (result << BigInt(16)) | BigInt(`0x${group}`)
  }
  return result
}

/**
 * Check if an IPv6 address is within a CIDR range
 */
function isIpv6InCidr(ip: string, cidr: string): boolean {
  const cidrParts = cidr.split('/')
  if (cidrParts.length !== 2 || !CIDR_PREFIX_PATTERN.test(cidrParts[1])) return false
  const prefix = Number(cidrParts[1])
  if (!Number.isInteger(prefix) || prefix < 0 || prefix > 128) return false
  const ipNum = parseIpv6(ip)
  const rangeNum = parseIpv6(cidrParts[0])
  if (ipNum === null || rangeNum === null) return false
  if (prefix === 0) return true
  const shift = BigInt(128 - prefix)
  return (ipNum >> shift) === (rangeNum >> shift)
}

/**
 * Check if an IP address is a Cloudflare IP
 */
export function isCloudflareIp(ip: string): boolean {
  if (!ip || ip !== ip.trim()) return false

  // Check if IPv6
  if (ip.includes(':')) {
    return CLOUDFLARE_IPV6_RANGES.some(cidr => {
      try {
        return isIpv6InCidr(ip, cidr)
      } catch {
        return false
      }
    })
  }

  // IPv4
  return CLOUDFLARE_IPV4_RANGES.some(cidr => {
    try {
      return isIpv4InCidr(ip, cidr)
    } catch {
      return false
    }
  })
}
