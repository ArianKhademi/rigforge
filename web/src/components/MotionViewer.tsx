import { Grid, OrbitControls, useGLTF } from "@react-three/drei";
import { Canvas, useFrame } from "@react-three/fiber";
import { Suspense, useEffect, useMemo } from "react";
import { AnimationMixer, SkeletonHelper, type Object3D, type SkinnedMesh } from "three";
import type { PlaybackClock } from "../lib/clock";

interface Props {
  url: string;
  clock: PlaybackClock;
  showSkeleton: boolean;
  /** Called once the asset is loaded, with what was found in it. */
  onLoaded?: (info: { bones: number; hasMesh: boolean; duration: number }) => void;
}

/** The 3D view of a motion asset: motion.glb played back on the shared clock. */
export function MotionViewer(props: Props) {
  return (
    <Canvas camera={{ position: [0.6, 1.35, 4.2], fov: 38 }} dpr={[1, 2]} shadows>
      <color attach="background" args={["#101216"]} />
      <hemisphereLight args={["#ffffff", "#30343c", 1.1]} />
      <directionalLight position={[2.5, 5, 3.5]} intensity={2.2} castShadow shadow-mapSize={[1024, 1024]} />
      <Suspense fallback={null}>
        <Motion {...props} />
      </Suspense>
      {/* A floor at y = 0: the worker grounds the character's feet there. */}
      <mesh rotation-x={-Math.PI / 2} receiveShadow>
        <planeGeometry args={[40, 40]} />
        <shadowMaterial opacity={0.35} />
      </mesh>
      <Grid
        infiniteGrid
        cellSize={0.25}
        sectionSize={1}
        cellColor="#262a33"
        sectionColor="#3a404d"
        fadeDistance={16}
        fadeStrength={1.5}
        position={[0, 0.001, 0]}
      />
      <OrbitControls makeDefault target={[0, 0.95, 0]} enableDamping minDistance={1.2} maxDistance={12} />
    </Canvas>
  );
}

function Motion({ url, clock, showSkeleton, onLoaded }: Props) {
  const gltf = useGLTF(url);

  const { mixer, helper, hasMesh } = useMemo(() => {
    let meshFound = false;
    gltf.scene.traverse((object: Object3D) => {
      const mesh = object as SkinnedMesh;
      if (mesh.isSkinnedMesh) {
        meshFound = true;
        mesh.castShadow = true;
        // Three culls a skinned mesh by its bind-pose bounds, which a moving
        // character leaves; without this it vanishes when it walks or jumps.
        mesh.frustumCulled = false;
      }
    });
    return { mixer: new AnimationMixer(gltf.scene), helper: new SkeletonHelper(gltf.scene), hasMesh: meshFound };
  }, [gltf]);

  useEffect(() => {
    const clip = gltf.animations[0];
    if (!clip) return;
    mixer.clipAction(clip).play();
    onLoaded?.({ bones: helper.bones.length, hasMesh, duration: clip.duration });
    return () => {
      mixer.stopAllAction();
      mixer.uncacheRoot(gltf.scene);
    };
    // onLoaded is intentionally not a dependency: it reports once per asset.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gltf, mixer, helper, hasMesh]);

  // Every rendered frame: advance the clock, then pose the character at the
  // clock's time. The mixer never runs on its own; it is always told the time,
  // which is what keeps the motion locked to the video.
  useFrame((_, delta) => {
    clock.tick(delta);
    mixer.setTime(clock.now());
  });

  return (
    <>
      <primitive object={gltf.scene} />
      {/* A skeleton-only asset has no mesh, so its bones are always drawn. */}
      <primitive object={helper} visible={showSkeleton || !hasMesh} />
    </>
  );
}
