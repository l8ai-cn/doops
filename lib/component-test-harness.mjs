import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import { createRequire } from "node:module"
import { setImmediate } from "node:timers/promises"
import TypeScript from "typescript"

const require = createRequire(import.meta.url)
export const componentStubs = new Proxy({}, { get: (_target, name) => name })

// Exercise the source component's callbacks and render props without a browser.
// Hooks keep their state/ref identities; browser layout and effects of child widgets are tested separately.
export function componentHarness(file, name, props, dependencies, browser = { setTimeout: () => 0 }) {
  const slots = []
  let cursor = 0
  let changed = true
  let tree
  const effects = []
  function memo(factory, deps) {
    const index = cursor++
    const previous = slots[index]
    if (!previous || !deps || deps.some((value, i) => !Object.is(value, previous.deps[i]))) {
      slots[index] = { value: factory(), deps }
    }
    return slots[index].value
  }
  const hooks = {
    useState(initial) {
      const index = cursor++
      if (!(index in slots)) slots[index] = typeof initial === "function" ? initial() : initial
      return [slots[index], (next) => {
        const value = typeof next === "function" ? next(slots[index]) : next
        if (!Object.is(value, slots[index])) changed = true
        slots[index] = value
      }]
    },
    useRef: (initial) => memo(() => ({ current: initial }), []),
    useMemo: memo,
    useCallback: (callback, deps) => memo(() => callback, deps),
    useEffect: (effect, deps) => memo(() => { effects.push(effect) }, deps),
    useSyncExternalStore: (_subscribe, snapshot) => snapshot(),
  }
  const filename = new URL(file, import.meta.url)
  const code = TypeScript.transpile(readFileSync(filename, "utf8"), {
    module: TypeScript.ModuleKind.CommonJS,
    target: TypeScript.ScriptTarget.ES2022,
    jsx: TypeScript.JsxEmit.ReactJSX,
  }, filename.pathname)
  const component = new Function("require", "exports", "window", "localStorage", `${code}\nreturn ${name}`)(
    (specifier) => {
      if (specifier === "react") return hooks
      if (specifier === "react/jsx-runtime") return require(specifier)
      assert.ok(specifier in dependencies, `Unmocked dependency: ${specifier}`)
      return dependencies[specifier]
    },
    {},
    browser,
    browser.localStorage,
  )
  const view = {
    render() {
      cursor = 0
      changed = false
      tree = component(props)
      return tree
    },
    async flush() {
      for (let attempt = 0; attempt < 20; attempt++) {
        view.render()
        for (const effect of effects.splice(0)) effect()
        await setImmediate()
        if (!changed && !effects.length) return
      }
      assert.fail("Component did not settle")
    },
    nodes(predicate) {
      return findElements(tree, predicate)
    },
    one(predicate) {
      const found = view.nodes(predicate)
      assert.equal(found.length, 1, "Expected exactly one matching element")
      return found[0]
    },
  }
  return view
}

export function findElements(tree, predicate) {
  const found = []
  function visit(node) {
    if (Array.isArray(node)) return node.forEach(visit)
    if (!node || typeof node !== "object") return
    if (predicate(node)) found.push(node)
    visit(node.props?.children)
  }
  visit(tree)
  return found
}

export function elementText(node) {
  if (Array.isArray(node)) return node.map(elementText).join("")
  if (node === null || node === undefined || typeof node === "boolean") return ""
  return typeof node === "object" ? elementText(node.props?.children) : String(node)
}

export function buttonNamed(name) {
  return (node) => (node.type === "button" || node.type === "Button") &&
    (elementText(node).trim() === name || node.props.title === name)
}
