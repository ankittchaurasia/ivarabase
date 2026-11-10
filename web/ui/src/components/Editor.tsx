import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap } from "@codemirror/autocomplete";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { javascript } from "@codemirror/lang-javascript";
import { json } from "@codemirror/lang-json";
import { PostgreSQL, sql } from "@codemirror/lang-sql";
import { bracketMatching, foldGutter, HighlightStyle, indentOnInput, syntaxHighlighting } from "@codemirror/language";
import { highlightSelectionMatches, searchKeymap } from "@codemirror/search";
import { Compartment, EditorState } from "@codemirror/state";
import { drawSelection, EditorView, highlightActiveLine, highlightActiveLineGutter, keymap, lineNumbers, placeholder as ph } from "@codemirror/view";
import { tags as t } from "@lezer/highlight";
import { useEffect, useRef } from "react";
import { cn } from "./ui";

// Syntax colors are CSS variables, so the editor follows the light/dark palette.
const v = (name: string) => `hsl(var(--syn-${name}))`;
const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.operatorKeyword, t.modifier, t.controlKeyword], color: v("keyword"), fontWeight: "500" },
  { tag: [t.string, t.special(t.string), t.regexp], color: v("string") },
  { tag: [t.number, t.bool, t.null, t.atom], color: v("number") },
  { tag: [t.comment, t.lineComment, t.blockComment], color: v("comment"), fontStyle: "italic" },
  { tag: [t.function(t.variableName), t.function(t.propertyName)], color: v("fn") },
  { tag: [t.typeName, t.className, t.standard(t.name)], color: v("type") },
  { tag: [t.propertyName, t.attributeName], color: v("prop") },
  { tag: [t.punctuation, t.bracket], color: "hsl(var(--muted-foreground))" },
]);

export type Lang = "sql" | "ts" | "json";

function language(lang: Lang, schema?: Record<string, unknown>) {
  if (lang === "sql") return sql({ dialect: PostgreSQL, schema: (schema as any) ?? {}, upperCaseKeywords: false });
  if (lang === "json") return json();
  return javascript({ typescript: true });
}

export interface EditorProps {
  value: string;
  onChange?: (v: string) => void;
  lang: Lang;
  schema?: Record<string, unknown>;
  onRun?: () => void;
  onSave?: () => void;
  readOnly?: boolean;
  placeholder?: string;
  className?: string;
}

/** CodeMirror 6 with Postgres/TypeScript highlighting, autocomplete and run/save keys. */
export function Editor({ value, onChange, lang, schema, onRun, onSave, readOnly, placeholder, className }: EditorProps) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView>();
  const langComp = useRef(new Compartment());
  const cbs = useRef({ onChange, onRun, onSave });
  cbs.current = { onChange, onRun, onSave };

  useEffect(() => {
    const v = new EditorView({
      parent: host.current!,
      state: EditorState.create({
        doc: value,
        extensions: [
          keymap.of([
            { key: "Mod-Enter", run: () => (cbs.current.onRun?.(), true) },
            { key: "Mod-s", preventDefault: true, run: () => (cbs.current.onSave?.(), true) },
          ]),
          lineNumbers(),
          highlightActiveLineGutter(),
          foldGutter(),
          history(),
          drawSelection(),
          indentOnInput(),
          bracketMatching(),
          closeBrackets(),
          autocompletion(),
          highlightActiveLine(),
          highlightSelectionMatches(),
          syntaxHighlighting(highlight),
          keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, ...completionKeymap, indentWithTab]),
          langComp.current.of(language(lang, schema)),
          EditorView.lineWrapping,
          EditorState.readOnly.of(!!readOnly),
          placeholder ? ph(placeholder) : [],
          EditorView.updateListener.of((u) => u.docChanged && cbs.current.onChange?.(u.state.doc.toString())),
        ],
      }),
    });
    view.current = v;
    return () => v.destroy();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // external value changes (e.g. picking from history)
  useEffect(() => {
    const v = view.current;
    if (v && value !== v.state.doc.toString()) v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: value } });
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({ effects: langComp.current.reconfigure(language(lang, schema)) });
  }, [lang, schema]);

  return <div ref={host} className={cn("overflow-hidden rounded-md border bg-card", className)} />;
}
