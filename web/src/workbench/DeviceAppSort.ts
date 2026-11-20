import { pinyin } from 'pinyin-pro'

export type DeviceApp = { packageName: string; thirdParty: boolean; name?: string; icon?: string }
export type AppGroup = { initial: string; apps: DeviceApp[] }

const appCollator = new Intl.Collator('en', { numeric: true, sensitivity: 'base' })

export function groupApps(apps: DeviceApp[]): AppGroup[] {
  const rows = apps.map(app => {
    const label = (app.name || app.packageName).trim()
    const sort = pinyin(label, { toneType: 'none', type: 'array' }).join('').normalize('NFD').replace(/\p{M}/gu, '')
    const letter = sort.charAt(0).toUpperCase()
    return { app, sort, initial: /^[A-Z]$/.test(letter) ? letter : '#' }
  })
  rows.sort((a, b) =>
    Number(a.initial === '#') - Number(b.initial === '#')
    || appCollator.compare(a.initial, b.initial)
    || appCollator.compare(a.sort, b.sort)
    || appCollator.compare(a.app.packageName, b.app.packageName))
  const groups: AppGroup[] = []
  for (const row of rows) {
    const last = groups.at(-1)
    if (last?.initial === row.initial) last.apps.push(row.app)
    else groups.push({ initial: row.initial, apps: [row.app] })
  }
  return groups
}
