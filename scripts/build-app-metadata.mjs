// Run with Android SDK platform 34, Build Tools 34.0.0 and JDK >=17.
// The checked-in text DEX keeps the ordinary Windows and Docker builds SDK-free.
import { execFileSync } from 'node:child_process'
import { mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const sdk = process.env.ANDROID_HOME || process.env.ANDROID_SDK_ROOT
const jdk = process.env.JAVA_HOME
if (!sdk || !jdk) throw new Error('ANDROID_HOME and JAVA_HOME must point to the SDK and JDK')
const android = path.join(sdk, 'platforms', 'android-34', 'android.jar')
const d8 = path.join(sdk, 'build-tools', '34.0.0', 'lib', 'd8.jar')
const source = fileURLToPath(new URL('../internal/platform/appmetadata/Main.java', import.meta.url))
const temp = mkdtempSync(path.join(tmpdir(), 'droidharbor-app-metadata-'))
const java = name => path.join(jdk, 'bin', name + (process.platform === 'win32' ? '.exe' : ''))
try {
  execFileSync(java('javac'), ['--release', '8', '-g:none', '-cp', android, '-d', temp, source], { stdio: ['ignore', 'ignore', 'inherit'] })
  const classes = path.join(temp, 'top/e404/droidharbor/appmetadata')
  execFileSync(java('java'), ['-cp', d8, 'com.android.tools.r8.D8', '--release', '--min-api', '24', '--lib', android, '--output', temp,
    ...readdirSync(classes).filter(name => name.endsWith('.class')).sort().map(name => path.join(classes, name))], { stdio: ['ignore', 'ignore', 'inherit'] })
  const dex = readFileSync(path.join(temp, 'classes.dex')).toString('base64')
  process.stdout.write(dex.match(/.{1,76}/g).join('\n') + '\n')
} finally {
  rmSync(temp, { recursive: true, force: true })
}
