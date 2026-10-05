import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef } from "react";
import { useApi } from "../api/context";
import type { Character } from "../api/types";
import styles from "./CharacterPicker.module.css";

const DESCRIPTIONS: Record<string, string> = {
  default: "The bundled mannequin, skinned to the Rigforge skeleton.",
  none: "No mesh: the animated 17-joint skeleton on its own.",
};

interface Props {
  value: string;
  onChange: (characterId: string) => void;
  disabled?: boolean;
}

/**
 * Choose the character the motion is applied to: a built-in, or a GLB the
 * user uploads. The api rejects a GLB whose skeleton is not the Rigforge
 * skeleton and says exactly why; that message is shown as-is.
 */
export function CharacterPicker({ value, onChange, disabled }: Props) {
  const api = useApi();
  const queryClient = useQueryClient();
  const fileInput = useRef<HTMLInputElement>(null);

  const characters = useQuery({ queryKey: ["characters"], queryFn: () => api.listCharacters() });

  const upload = useMutation({
    mutationFn: (file: File) => api.uploadCharacter(file, file.name),
    onSuccess: (created) => {
      queryClient.setQueryData<Character[]>(["characters"], (list) => [...(list ?? []), created]);
      onChange(created.id);
    },
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteCharacter(id),
    onSuccess: (_, id) => {
      queryClient.setQueryData<Character[]>(["characters"], (list) => list?.filter((c) => c.id !== id));
      if (value === id) onChange("default");
    },
  });

  return (
    <fieldset className={styles.fieldset} disabled={disabled}>
      <legend>Character</legend>
      {characters.error && <p className="notice notice-error">Could not load characters: {characters.error.message}</p>}
      <div className={styles.options} role="radiogroup" aria-label="Character">
        {(characters.data ?? []).map((character) => (
          <label
            key={character.id}
            className={`${styles.option} ${value === character.id ? styles.selected : ""}`}
          >
            <input
              type="radio"
              name="character"
              value={character.id}
              checked={value === character.id}
              onChange={() => onChange(character.id)}
            />
            <span className={styles.text}>
              <span className={styles.name}>{character.name}</span>
              <span className={styles.description}>
                {character.builtin ? DESCRIPTIONS[character.id] : "Your character (GLB)."}
              </span>
            </span>
            {!character.builtin && (
              <button
                type="button"
                className="btn btn-ghost"
                aria-label={`Remove ${character.name}`}
                onClick={(event) => {
                  event.preventDefault();
                  remove.mutate(character.id);
                }}
              >
                Remove
              </button>
            )}
          </label>
        ))}
      </div>

      <div className={styles.uploadRow}>
        <button type="button" className="btn" onClick={() => fileInput.current?.click()} disabled={upload.isPending}>
          {upload.isPending ? "Checking rig…" : "Use your own character (.glb)"}
        </button>
        <span className={styles.hint}>Must be skinned to the 17-joint Rigforge skeleton.</span>
        <input
          ref={fileInput}
          type="file"
          accept=".glb,model/gltf-binary"
          hidden
          data-testid="character-file"
          onChange={(event) => {
            const file = event.target.files?.[0];
            if (file) upload.mutate(file);
            event.target.value = ""; // allow choosing the same file again
          }}
        />
      </div>
      {upload.error && (
        <p className="notice notice-error" role="alert" data-testid="character-error">
          {upload.error.message}
        </p>
      )}
    </fieldset>
  );
}
